"""juju-lens tap: records what a Juju hook process does, into a spool file.

`juju-lens-probe --unit` installs this module into every charm venv on its
host for the duration of a `juju-lens record --attach unit`, with a loader
that imports it when the charm's Python starts: a .pth file in the venv's
site-packages, or a sitecustomize module for charms that put the venv
directory on PYTHONPATH. The probe removes both, and the spool, when the
recording ends.

The module does nothing at all unless every one of these holds:

- the process has a Juju hook environment (JUJU_DISPATCH_PATH,
  JUJU_HOOK_NAME, JUJU_ACTION_NAME or JUJU_CONTEXT_ID);
- the spool directory exists, is a real directory (not a symlink), is owned
  by the effective uid and has mode 0700;
- the probe touched the spool's heartbeat file in the last 30 seconds, so a
  probe that died without cleaning up leaves an inert module behind;
- the spool's config file, read once here, turns at least one feature on.

So the features of one hook are fixed when it starts, and the probe can
change them from one hook to the next by rewriting the config.

Features:

- "proc": proc_start and proc_end records, the boundaries of the process.
- "ops": what ops does inside the process: events, observers, deferrals,
  hook tools and Pebble calls.
- "stacks": a thread that samples the main thread's stack every 10 ms, each
  sample tagged with the event or observer running (see below).

Records are JSON lines in <spool>/<unit>-<pid>-<start ns>.jsonl, each
written with one write(2) to an O_APPEND file, so a reader sees complete
lines as soon as they are written. Every record has t (wall-clock ns), m
(monotonic ns) and k (its kind). Values that can hold credentials (databag
and config values, secret contents, hook-tool output, Pebble file
contents) are never recorded, only keys and byte sizes; exception messages
and a failing hook tool's stderr are cut to 300 characters.

How "ops" works: a sys.meta_path finder wraps the loader of a few ops
modules and patches them right after the module body executed, before
anything can bind their names:

  ops               -> ops_loaded record
  ops.framework     -> Framework._emit, Framework._reemit, Framework.observe, EventBase.defer
  ops.pebble        -> Client._request_raw
  ops.model, ops.storage, ops.hookcmds._utils, ops.main, ops._main
                    -> the module-global ``subprocess`` (or ``run``) seen by that module is
                       replaced by a recording proxy, so every hook-tool call ops makes is seen
                       whatever ops layer issued it (_ModelBackend._run, hookcmds, JujuStorage)
  ops.main, ops._main -> ``main`` (dispatch_end with exit code / exception)

Observer calls: ops calls observers as ``getattr(observer_obj, method_name)(event)`` inside
Framework._reemit. After each Framework.observe() the tap binds a recording wrapper as an
instance attribute (a bound method, so re-observing the same handler still passes ops'
``isinstance(observer, MethodType)`` check). The wrapper only records when called from
Framework._reemit; direct calls by charm code are passed through silently. A patch that
fails is reported with a tap_warn record and left out; the hook runs as without it.

ops' own tracing (ops[tracing]) is not used and not touched: a charm that sets it up keeps
exporting the same spans.

Span numbers: dispatch_start (charm init, until ops emits the hook's event or re-emits
deferred ones), reemit_start, emit and observer_start each open a span and carry its number,
sp, unique in the process; it closes with the matching end record. 0 is the process itself.
The records that open and close spans, and proc_start and proc_end, also carry the process's
resident memory (rss) and its high-water mark (hwm, ru_maxrss), in bytes.

How "stacks" works: a daemon thread wakes every 10 ms, reads the main thread's stack
(sys._current_frames), the main thread's CPU clock and the process's resident memory, and
the innermost open span. Samples with the same span and stack add up into one row: count,
wall time, CPU time and memory growth since the previous sample. Each sample also goes to a
timeline of (time, row, RSS). About once a second, and at exit, the thread writes what is
new as a "stacks" record: frames and rows first seen, row totals since the last record, and
the timeline; the last one is followed by "stacks_end". So a hook killed mid-way keeps what
was written up to its last second. The tap's own frames are left out of stacks.

Standard library only, Python 3.8 and later. It never raises into the
charm, never writes to stdout or stderr, and never calls juju-log.
"""

import os
import sys

# JUJU_LENS_TAP_DIR moves the spool, for tests; the probe uses the default.
_SPOOL = os.environ.get('JUJU_LENS_TAP_DIR') or '/tmp/juju-lens-tap'
_HEARTBEAT = 'alive'
_HEARTBEAT_MAX_AGE = 30.0  # seconds; the probe touches it every second
_CONFIG = 'config.json'
_CONFIG_MAX_BYTES = 64 << 10
_KNOWN_FEATURES = ('proc', 'ops', 'stacks')
_ENV_TRIGGERS = ('JUJU_DISPATCH_PATH', 'JUJU_HOOK_NAME', 'JUJU_ACTION_NAME', 'JUJU_CONTEXT_ID')
_MARK = '_juju_lens_tap'
_fd = -1


def _hook_process():
    env = os.environ
    return any(env.get(k) for k in _ENV_TRIGGERS)


def _spool_ok():
    import stat
    import time

    try:
        st = os.lstat(_SPOOL)
        if not (
            stat.S_ISDIR(st.st_mode)
            and st.st_uid == os.geteuid()
            and stat.S_IMODE(st.st_mode) == 0o700
        ):
            return False
        hb = os.lstat(os.path.join(_SPOOL, _HEARTBEAT))
    except OSError:
        return False
    return stat.S_ISREG(hb.st_mode) and abs(time.time() - hb.st_mtime) <= _HEARTBEAT_MAX_AGE


def _read_features():
    """The features the config turns on, as a set; empty if it is unreadable."""
    import json

    try:
        fd = os.open(os.path.join(_SPOOL, _CONFIG), os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
        try:
            raw = os.read(fd, _CONFIG_MAX_BYTES + 1)
        finally:
            os.close(fd)
        if len(raw) > _CONFIG_MAX_BYTES:
            return set()
        cfg = json.loads(raw.decode('utf-8'))
        return {f for f in cfg.get('features', ()) if f in _KNOWN_FEATURES}
    except Exception:
        return set()


try:
    _FEATURES = _read_features() if _hook_process() and _spool_ok() else set()
except Exception:
    _FEATURES = set()

if _FEATURES:
    import json
    import time
    import types

    _ns = time.time_ns
    _mono = time.monotonic_ns
    _dumps = json.JSONEncoder(separators=(',', ':'), default=str, ensure_ascii=False).encode
    _write = os.write
    _getframe = sys._getframe

    def _open():
        global _fd
        unit = os.environ.get('JUJU_UNIT_NAME', 'unknown').replace('/', '-')
        name = '%s-%d-%d.jsonl' % (unit, os.getpid(), _ns())
        _fd = os.open(
            os.path.join(_SPOOL, name),
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_APPEND | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )

    def _rec(kind, **fields):
        # One write(2) per record: the file is O_APPEND and unbuffered, so a reader sees each
        # record as soon as it is produced. Errors are swallowed: the tap must never break a hook.
        try:
            d = {'t': _ns(), 'm': _mono(), 'k': kind}
            d.update(fields)
            _write(_fd, (_dumps(d) + '\n').encode('utf-8', 'replace'))
        except Exception:
            pass

    def _trunc(s, n=300):
        try:
            s = str(s)
        except Exception:
            return '<unprintable>'
        return s if len(s) <= n else s[:n] + '...(%d)' % len(s)

    def _exc_fields(e):
        return {'exc': type(e).__qualname__, 'exc_msg': _trunc(e)}

    # ---------------------------------------------------------------- memory
    # Every record that opens or closes a span carries the process's resident memory
    # (rss, from /proc/self/statm through one open fd) and its high-water mark (hwm,
    # ru_maxrss, which the kernel keeps exact), both in bytes. A span whose end hwm is above
    # its start hwm peaked at exactly that. One reading costs about 2 microseconds.
    try:
        import resource

        _statm = os.open('/proc/self/statm', os.O_RDONLY | os.O_CLOEXEC)
        _page = os.sysconf('SC_PAGE_SIZE')
        _pread = os.pread
        _getrusage, _RUSAGE_SELF = resource.getrusage, resource.RUSAGE_SELF
    except Exception:
        _statm = -1

    def _rss():
        try:
            return int(_pread(_statm, 64, 0).split()[1]) * _page
        except Exception:
            return 0

    def _mem():
        """{'rss': bytes, 'hwm': bytes}, or {} where they can't be read."""
        if _statm < 0:
            return {}
        try:
            return {'rss': int(_pread(_statm, 64, 0).split()[1]) * _page,
                    'hwm': _getrusage(_RUSAGE_SELF).ru_maxrss << 10}
        except Exception:
            return {}

    # ---------------------------------------------------------------- process records
    _ENV_KEYS = (
        'JUJU_UNIT_NAME', 'JUJU_MODEL_NAME', 'JUJU_MODEL_UUID', 'JUJU_DISPATCH_PATH',
        'JUJU_HOOK_NAME', 'JUJU_ACTION_NAME', 'JUJU_ACTION_UUID', 'JUJU_RELATION',
        'JUJU_RELATION_ID', 'JUJU_REMOTE_UNIT', 'JUJU_REMOTE_APP', 'JUJU_DEPARTING_UNIT',
        'JUJU_SECRET_ID', 'JUJU_SECRET_LABEL', 'JUJU_SECRET_REVISION', 'JUJU_NOTICE_ID',
        'JUJU_NOTICE_TYPE', 'JUJU_NOTICE_KEY', 'JUJU_WORKLOAD_NAME', 'JUJU_PEBBLE_CHECK_NAME',
        'JUJU_STORAGE_ID', 'JUJU_CHARM_DIR', 'JUJU_CONTEXT_ID', 'JUJU_VERSION',
        'JUJU_DEBUG_AT',
    )

    def _cmdline():
        try:
            with open('/proc/self/cmdline', 'rb') as f:
                return [a.decode('utf-8', 'replace') for a in f.read().split(b'\0') if a]
        except Exception:
            return list(sys.argv)

    _state = {'exit': None, 'main_depth': 0, 'init': None}

    # ---------------------------------------------------------------- span numbers
    # The open spans, innermost last. Only the main thread changes the list; the stack
    # sampler reads its last item. 0, the process, is never removed.
    _spans = [0]
    _span_seq = [0]

    def _span_open():
        _span_seq[0] += 1
        n = _span_seq[0]
        _spans.append(n)
        return n

    def _span_close(n):
        # Closes n, and whatever opened inside it and lost its end.
        try:
            i = _spans.index(n)
        except ValueError:
            return
        del _spans[i:]

    def _end_init():
        n = _state['init']
        if n is not None:
            _state['init'] = None
            _span_close(n)

    # ---------------------------------------------------------------- stacks
    _STACK_INTERVAL = 0.01  # seconds between samples
    _STACK_FLUSH_NS = 1000000000  # a stacks record about once a second
    _STACK_FLUSH_FRAMES = 2000  # or sooner, to keep records small
    _STACK_MAX_STACKS = 5000  # distinct (span, stack) pairs; later new ones are only counted
    _STACK_MAX_DEPTH = 128
    _STACK_MAX_NS = 3600 * 1000000000

    class _Stacks(object):
        """Samples the main thread's stack from a daemon thread; see the module docstring."""

        def __init__(self):
            import threading

            self.pid = os.getpid()
            self.tid = threading.get_ident()
            try:
                self.clock = time.pthread_getcpuclockid(self.tid)
            except Exception:
                self.clock = None
            self.own = _exc_fields.__code__.co_filename  # the tap's frames are left out
            self.index = {}  # code object -> frame id; None for the tap's own code
            self.nframes = 0
            self.frames = []  # [file, function, first line], first seen since the last record
            self.stacks = {}  # (span, frame id, ...) -> stack id
            self.new_stacks = []  # [span, [frame ids, innermost first]], since the last record
            self.totals = {}  # stack id -> [samples, wall ns, cpu ns, memory growth], same
            self.timeline = []  # flat: µs since m0, stack id (-1: not kept), RSS in KiB
            self.f0 = self.s0 = 0
            self.samples = self.dropped = 0
            self.own_cpu = 0
            self.stopped = None
            self.lock = threading.Lock()
            self.stop = threading.Event()
            self.m0 = _mono()
            self.rss = self.rss_start = self.rss_peak = _rss()
            self.thread = threading.Thread(target=self._run, name='juju-lens-stacks')
            self.thread.daemon = True

        def _cpu(self):
            if self.clock is None:
                return 0
            try:
                return time.clock_gettime_ns(self.clock)
            except Exception:
                return 0

        def _run(self):
            try:
                self._loop()
            except Exception:
                pass
            finally:
                self.own_cpu += time.thread_time_ns()

        def _loop(self):
            mono, current_frames, wait, spans = _mono, sys._current_frames, self.stop.wait, _spans
            last_m, last_cpu = mono(), self._cpu()
            flush_at = last_m + _STACK_FLUSH_NS
            deadline = self.m0 + _STACK_MAX_NS
            while not wait(_STACK_INTERVAL):
                frame = current_frames().get(self.tid)
                if frame is None or self.stop.is_set():
                    return  # the main thread is gone, or in our atexit hook
                now = mono()
                cpu, rss = self._cpu(), _rss()
                with self.lock:
                    self._sample(frame, spans[-1], now, now - last_m, max(cpu - last_cpu, 0), rss)
                # Hold no reference to the main thread's frames between samples, so the
                # charm's locals are freed when it is done with them.
                frame = None
                last_m, last_cpu = now, cpu
                if now >= flush_at or len(self.frames) >= _STACK_FLUSH_FRAMES:
                    self.flush()
                    flush_at = now + _STACK_FLUSH_NS
                if now >= deadline:
                    self.stopped = 'max_duration'
                    return

        def _sample(self, frame, span, now, wall, cpu, rss):
            key = [span]
            index, own = self.index, self.own
            while frame is not None and len(key) <= _STACK_MAX_DEPTH:
                code = frame.f_code
                frame = frame.f_back
                i = index.get(code, -1)
                if i == -1:
                    if code.co_filename == own:
                        i = None
                    else:
                        i = self.nframes
                        self.nframes += 1
                        self.frames.append([code.co_filename, getattr(code, 'co_qualname', code.co_name),
                                            code.co_firstlineno])
                    index[code] = i
                if i is not None:
                    key.append(i)
            key = tuple(key)
            mem = max(rss - self.rss, 0)
            self.rss = rss
            if rss > self.rss_peak:
                self.rss_peak = rss
            self.samples += 1
            sid = self.stacks.get(key)
            if sid is None:
                if len(self.stacks) >= _STACK_MAX_STACKS:
                    self.dropped += 1
                    sid = -1
                else:
                    sid = self.stacks[key] = len(self.stacks)
                    self.new_stacks.append([span, list(key[1:])])
            if sid >= 0:
                t = self.totals.get(sid)
                if t is None:
                    self.totals[sid] = [1, wall, cpu, mem]
                else:
                    t[0] += 1
                    t[1] += wall
                    t[2] += cpu
                    t[3] += mem
            self.timeline.extend(((now - self.m0) // 1000, sid, rss >> 10))

        def flush(self):
            with self.lock:
                if not (self.timeline or self.frames or self.new_stacks):
                    return
                rec = {'m0': self.m0, 'f0': self.f0, 'frames': self.frames, 's0': self.s0,
                       'stacks': self.new_stacks, 'totals': [[s] + t for s, t in self.totals.items()],
                       'tl': self.timeline}
                self.f0 += len(self.frames)
                self.s0 += len(self.new_stacks)
                self.frames, self.new_stacks, self.totals, self.timeline = [], [], {}, []
                _rec('stacks', **rec)

        def finish(self):
            if os.getpid() != self.pid:
                return  # a forked child: the parent writes the stacks
            cpu0 = time.thread_time_ns()
            self.stop.set()
            self.thread.join(0.5)
            self.flush()
            f = {'samples': self.samples, 'dropped': self.dropped,
                 'interval_ns': int(_STACK_INTERVAL * 1e9), 'rss_start': self.rss_start,
                 'rss_peak': self.rss_peak, 'cpu_clock': self.clock is not None}
            if self.stopped:
                f['stopped'] = self.stopped
            # The sampler's own CPU time: its thread, and this.
            f['cpu_ns'] = self.own_cpu + time.thread_time_ns() - cpu0
            _rec('stacks_end', **f)

    def _stacks_at_exit(s):
        try:
            s.finish()
        except Exception:
            pass

    def _at_exit():
        f = {'pid': os.getpid()}
        if _state['exit'] is not None:
            f.update(_state['exit'])
        else:
            # Uncaught exception outside ops.main (or main not wrapped): PyErr_Print sets
            # sys.last_* before the interpreter runs atexit handlers.
            et = getattr(sys, 'last_type', None)
            if et is not None:
                f['rc'] = 1
                f['exc'] = et.__qualname__
                f['exc_msg'] = _trunc(getattr(sys, 'last_value', ''))
        f.update(_mem())
        _rec('proc_end', **f)

    # ---------------------------------------------------------------- patch helpers
    _patched = []  # human-readable list of applied patches, reported in ops_loaded/patch recs

    def _wrapped(fn):
        return getattr(fn, _MARK, False)

    def _patch_attr(owner, name, make, label):
        orig = owner.__dict__.get(name) if isinstance(owner, type) else getattr(owner, name, None)
        if orig is None:
            _rec('tap_warn', what='patch target missing', target=label)
            return None
        if _wrapped(orig):
            return None
        new = make(orig)
        try:
            new.__wrapped__ = orig
            new.__name__ = getattr(orig, '__name__', name)
            new.__qualname__ = getattr(orig, '__qualname__', name)
            new.__doc__ = getattr(orig, '__doc__', None)
            new.__module__ = getattr(orig, '__module__', None)
            setattr(new, _MARK, True)
        except Exception:
            pass
        setattr(owner, name, new)
        _patched.append(label)
        return orig

    # ---------------------------------------------------------------- hook tools
    _MSG_TOOLS = {'juju-log': 1, 'action-log': 1}
    _KEEP_TOOLS = {'status-set', 'application-version-set', 'action-fail', 'open-port',
                   'close-port', 'juju-reboot', 'is-leader', 'relation-ids', 'relation-list',
                   'network-get', 'goal-state', 'storage-list', 'storage-get', 'resource-get',
                   'opened-ports', 'secret-ids', 'secret-info-get', 'status-get',
                   'state-get', 'leader-get', 'credential-get', 'config-get', 'action-get',
                   'secret-get', 'relation-get', 'relation-model-get', 'unit-get'}
    _STDIN_KEYS_TOOLS = {'relation-set', 'state-set', 'action-set', 'leader-set',
                         'secret-add', 'secret-set'}

    def _redact_args(tool, args):
        """Keep flags, keys and sizes; drop values that may carry relation data / secrets."""
        if tool == 'status-set':
            return list(args)
        out = []
        if tool in _MSG_TOOLS:
            # juju-log [--log-level L] [--] message: keep the level, size of the message.
            skip_next = False
            for a in args:
                if skip_next:
                    out.append(a)
                    skip_next = False
                elif a in ('-l', '--log-level'):
                    out.append(a)
                    skip_next = True
                elif a.startswith('-') and a != '-':
                    out.append(a)
                else:
                    out.append('<%d bytes>' % len(a.encode('utf-8', 'replace')))
            return out
        for a in args:
            if not isinstance(a, str):
                a = str(a)
            if a.startswith('--') and '=' in a and tool in _KEEP_TOOLS:
                out.append(a)  # e.g. --format=json
            elif '=' in a and not a.startswith('--format'):
                k, _, v = a.partition('=')
                out.append('%s=<%d bytes>' % (k, len(v.encode('utf-8', 'replace'))))
            elif tool in _KEEP_TOOLS or a.startswith('-'):
                out.append(_trunc(a, 200))
            else:
                # positional value of a writing tool (e.g. secret-set <id> key=..): keep short
                # identifiers, size-only otherwise
                out.append(a if len(a) <= 64 and ' ' not in a else '<%d bytes>' % len(a))
        return out

    def _stdin_keys(data):
        # Best effort: top-level keys of the YAML/JSON mapping ops writes to relation-set
        # --file - / state-set / action-set. Values are never recorded.
        try:
            if isinstance(data, bytes):
                data = data.decode('utf-8', 'replace')
            if data.lstrip()[:1] == '{':
                if len(data) > 65536:
                    return None  # don't pay a big json.loads on large databags
                obj = json.loads(data)
                return list(obj)[:64] if isinstance(obj, dict) else None
            keys = []
            for line in data.splitlines():
                if line and not line[0].isspace() and not line.startswith(('-', '#', '{')):
                    k = line.split(':', 1)[0].strip().strip('\'"')
                    if k:
                        keys.append(k)
                if len(keys) >= 64:
                    break
            return keys
        except Exception:
            return None

    def _size(x):
        if x is None:
            return 0
        if isinstance(x, (bytes, bytearray)):
            return len(x)
        try:
            return len(x.encode('utf-8', 'replace'))
        except Exception:
            return -1

    def _make_run(orig_run, origin):
        def run(*args, **kwargs):
            t0 = _mono()
            argv = args[0] if args else kwargs.get('args')
            res = None
            err = None
            try:
                res = orig_run(*args, **kwargs)
                return res
            except BaseException as e:
                err = e
                raise
            finally:
                try:
                    dur = _mono() - t0
                    if isinstance(argv, (list, tuple)) and argv:
                        tool = os.path.basename(str(argv[0]))
                        rest = [str(a) for a in argv[1:]]
                    else:
                        tool = os.path.basename(str(argv).split(' ', 1)[0])
                        rest = []
                    f = {'tool': tool, 'args': _redact_args(tool, rest), 'dur_ns': dur,
                         'via': origin}
                    inp = kwargs.get('input')
                    if inp is not None:
                        f['stdin_bytes'] = _size(inp)
                        if tool in _STDIN_KEYS_TOOLS:
                            f['stdin_keys'] = _stdin_keys(inp)
                    if res is not None:
                        f['rc'] = res.returncode
                        f['out_bytes'] = _size(res.stdout)
                        f['err_bytes'] = _size(res.stderr)
                    elif err is not None:
                        rc = getattr(err, 'returncode', None)
                        if rc is not None:
                            f['rc'] = rc
                            f['out_bytes'] = _size(getattr(err, 'stdout', None))
                            f['err_bytes'] = _size(getattr(err, 'stderr', None))
                            # stderr of a failing hook tool is an error message, not data
                            f['stderr'] = _trunc(
                                (err.stderr or b'').decode('utf-8', 'replace')
                                if isinstance(err.stderr, bytes) else err.stderr, 300)
                        else:
                            f.update(_exc_fields(err))
                    _rec('hook_tool', **f)
                    if tool == 'status-set':
                        # status-set [--application[=True|False]] <status> [--] [message]
                        # Flags only precede the status word (ops <= 3.3 omits '--', so a
                        # message may itself start with '-').
                        app = False
                        pos = []
                        for a in rest:
                            if pos:
                                if a != '--' or len(pos) > 1:
                                    pos.append(a)
                            elif a.startswith('--application'):
                                app = a.split('=', 1)[1].lower() == 'true' if '=' in a else True
                            elif not a.startswith('-'):
                                pos.append(a)
                        _rec('status_set', status=pos[0] if pos else None,
                             message=' '.join(pos[1:]), app=app, rc=f.get('rc'))
                except Exception:
                    pass
        return run

    def _patch_subprocess_user(mod):
        """Swap the subprocess entry points the given ops module sees for recording ones."""
        import subprocess as real

        origin = mod.__name__
        g = mod.__dict__
        if g.get('subprocess') is real:
            proxy = types.ModuleType('subprocess')
            proxy.__dict__.update(real.__dict__)
            proxy.run = _make_run(real.run, origin)
            setattr(proxy.run, _MARK, True)
            proxy.__dict__[_MARK] = True
            g['subprocess'] = proxy
            _patched.append(origin + '.subprocess')
        if g.get('run') is real.run:
            new = _make_run(real.run, origin)
            setattr(new, _MARK, True)
            g['run'] = new
            _patched.append(origin + '.run')

    # ---------------------------------------------------------------- framework
    _fw = {'reemit_code': None, 'stack': [], 'emit_depth': 0, 'lifecycle': None}

    def _lifecycle_types():
        lc = _fw['lifecycle']
        if lc is None:
            fwm = sys.modules.get('ops.framework')
            ch = sys.modules.get('ops.charm')
            if fwm is None or ch is None:
                return ()
            names = []
            for m, n in ((fwm, 'LifecycleEvent'), (fwm, 'PreCommitEvent'),
                         (fwm, 'CommitEvent'), (ch, 'CollectStatusEvent')):
                c = getattr(m, n, None)
                if c is not None:
                    names.append(c)
            lc = (tuple(names), getattr(ch, 'HookEvent', ()), getattr(ch, 'ActionEvent', ()))
            _fw['lifecycle'] = lc
        return lc

    def _classify(event):
        try:
            lc = _lifecycle_types()
            if not lc:
                return None
            lifecycle, hook, action = lc
            if action and isinstance(event, action):
                return 'action'
            if hook and isinstance(event, hook):
                return 'juju'
            if lifecycle and isinstance(event, lifecycle):
                return 'lifecycle'
            return 'custom'
        except Exception:
            return None

    def _make_emit(orig):
        def _emit(self, event, *a, **kw):
            t0 = _mono()
            path = None
            sp = None
            try:
                h = event.handle
                path = h.path
                parent_path = h.parent.path if h.parent is not None else None
                kind = h.kind
                obs = [
                    '%s.%s' % (op, mn) for (op, mn, pp, ek) in self._observers
                    if pp == parent_path and (not ek or ek == kind)
                ]
                cls = _classify(event)
                depth = _fw['emit_depth']
                if depth == 0 and cls in ('juju', 'action'):
                    _end_init()  # ops versions that don't re-emit deferred events first
                sp = _span_open()
                _rec('emit', ev=path, ev_kind=kind, ev_class=type(event).__qualname__,
                     cls=cls, observers=obs, depth=depth,
                     in_observer=_fw['stack'][-1]['obs'] if _fw['stack'] else None, sp=sp, **_mem())
            except Exception:
                pass
            _fw['emit_depth'] += 1
            exc = None
            try:
                return orig(self, event, *a, **kw)
            except BaseException as e:
                exc = e
                raise
            finally:
                _fw['emit_depth'] -= 1
                if sp is not None:
                    _span_close(sp)
                f = {'ev': path, 'dur_ns': _mono() - t0}
                f.update(_mem())
                if exc is not None:
                    f.update(_exc_fields(exc))
                _rec('emit_end', **f)
        return _emit

    def _make_reemit(orig):
        def _reemit(self, single_event_path=None, *a, **kw):
            if single_event_path is not None:
                return orig(self, single_event_path, *a, **kw)
            # Replay of the deferred backlog. Count notices by observing ops' own iteration
            # of storage.notices() (an extra query would cost a state-get on JujuStorage).
            t0 = _mono()
            count = [0]
            storage = getattr(self, '_storage', None)
            installed = False
            if storage is not None and 'notices' not in getattr(storage, '__dict__', {'notices': 1}):
                try:
                    orig_notices = storage.notices

                    def _count(it):
                        for n in it:
                            count[0] += 1
                            yield n

                    def notices(*na, **nk):
                        # Only ops' own first listing is the backlog: unhook immediately so
                        # nested emits (_reemit(path), _event_is_in_storage) are untouched.
                        try:
                            del storage.notices
                        except Exception:
                            pass
                        return _count(orig_notices(*na, **nk))

                    storage.notices = notices
                    installed = True
                except Exception:
                    pass
            _end_init()
            sp = _span_open()
            _rec('reemit_start', sp=sp, **_mem())
            exc = None
            try:
                return orig(self, single_event_path, *a, **kw)
            except BaseException as e:
                exc = e
                raise
            finally:
                if installed and storage.__dict__.get('notices') is notices:
                    try:
                        del storage.notices  # ops never listed notices (should not happen)
                    except Exception:
                        pass
                _span_close(sp)
                f = {'count': count[0] if installed else None, 'dur_ns': _mono() - t0}
                f.update(_mem())
                if exc is not None:
                    f.update(_exc_fields(exc))
                _rec('reemit_backlog', **f)
        return _reemit

    def _called_from_reemit():
        """(True, replay) iff our caller is Framework._reemit (directly or via pdb.runcall)."""
        code = _fw['reemit_code']
        try:
            f = _getframe(2)
            for _ in range(3):
                if f is None:
                    return False, None
                if f.f_code is code:
                    sep = f.f_locals.get('single_event_path', '')
                    return True, sep is None
                if f.f_code.co_name != 'runcall':  # JUJU_DEBUG_AT: _reemit -> pdb.runcall -> Bdb.runcall
                    return False, None
                f = f.f_back
        except Exception:
            pass
        return False, None

    def _install_observer_wrapper(obj, method_name):
        d = getattr(obj, '__dict__', None)
        if d is None:
            return False
        cur = d.get(method_name)
        if cur is not None and getattr(getattr(cur, '__func__', None), _MARK, False):
            return True
        bound = getattr(obj, method_name, None)
        if bound is None or not callable(bound):
            return False
        func = getattr(bound, '__func__', bound)
        qual = '%s.%s' % (getattr(func, '__module__', '?'), getattr(func, '__qualname__', method_name))
        try:
            obs_path = obj.handle.path
        except Exception:
            obs_path = None
        cls = type(obj)
        static = cur is not None  # the instance already had its own attribute: keep calling it

        def target():
            # Late binding: honour class-level patches made after observe() (e.g. mock.patch.object
            # on the charm class in unit tests), exactly like ops' getattr(observer, name) would.
            if static:
                return bound
            attr = getattr(cls, method_name, None)
            get = getattr(attr, '__get__', None)
            return get(obj, cls) if get is not None else bound

        def observer(self_, *a, **kw):
            # Signature-agnostic: charm code may call the handler directly with other arguments
            # (e.g. grafana_dashboard's _update_all_dashboards_from_dir(inject_dropdowns=...)).
            ok, replay = _called_from_reemit()
            if not ok or not a:
                return target()(*a, **kw)
            event = a[0]
            t0 = _mono()
            info = {'obs': qual, 'obs_path': obs_path}
            sp = _span_open()
            try:
                h = event.handle
                info['ev'] = h.path
                _rec('observer_start', obs=qual, obs_path=obs_path, method=method_name,
                     ev=h.path, ev_kind=h.kind, ev_class=type(event).__qualname__,
                     cls=_classify(event), reemit=bool(replay), depth=len(_fw['stack']), sp=sp, **_mem())
            except Exception:
                pass
            _fw['stack'].append(info)
            exc = None
            try:
                return target()(*a, **kw)
            except BaseException as e:
                exc = e
                raise
            finally:
                _fw['stack'].pop()
                _span_close(sp)
                f = {'obs': qual, 'ev': info.get('ev'), 'dur_ns': _mono() - t0,
                     'deferred': bool(getattr(event, 'deferred', False))}
                f.update(_mem())
                if exc is not None:
                    f.update(_exc_fields(exc))
                _rec('observer_end', **f)

        observer.__name__ = method_name
        observer.__qualname__ = getattr(func, '__qualname__', method_name)
        # inspect.signature(MethodType(observer, obj)) follows __wrapped__ and then drops the
        # first parameter, so it must point at the *unbound* function (self, event): older ops
        # (<= 2.x) validate observers with inspect.signature(observer) (follow_wrapped=True).
        observer.__wrapped__ = func
        setattr(observer, _MARK, True)
        d[method_name] = types.MethodType(observer, obj)
        return True

    def _make_observe(orig):
        def observe(self, bound_event, observer, *a, **kw):
            r = orig(self, bound_event, observer, *a, **kw)
            try:
                obj = getattr(observer, '__self__', None)
                name = getattr(observer, '__name__', None)
                if obj is not None and name and not _install_observer_wrapper(obj, name):
                    _rec('tap_warn', what='observer not wrappable',
                         obs='%s.%s' % (type(obj).__qualname__, name))
            except Exception:
                pass
            return r
        return observe

    def _make_defer(orig):
        def defer(self, *a, **kw):
            try:
                st = _fw['stack']
                _rec('defer', ev=self.handle.path, ev_class=type(self).__qualname__,
                     obs=st[-1]['obs'] if st else None)
            except Exception:
                pass
            return orig(self, *a, **kw)
        return defer

    def _patch_framework(mod):
        F = getattr(mod, 'Framework', None)
        E = getattr(mod, 'EventBase', None)
        if F is None or E is None:
            _rec('tap_warn', what='patch target missing', target='Framework/EventBase')
            return
        orig = _patch_attr(F, '_reemit', _make_reemit, 'Framework._reemit')
        if orig is not None:
            _fw['reemit_code'] = orig.__code__
        _patch_attr(F, '_emit', _make_emit, 'Framework._emit')
        _patch_attr(F, 'observe', _make_observe, 'Framework.observe')
        _patch_attr(E, 'defer', _make_defer, 'EventBase.defer')

    # ---------------------------------------------------------------- pebble
    def _make_request_raw(orig):
        def _request_raw(self, method, path, query=None, headers=None, data=None, *a, **kw):
            t0 = _mono()
            status = None
            exc = None
            try:
                resp = orig(self, method, path, query, headers, data, *a, **kw)
                status = getattr(resp, 'status', None)
                return resp
            except BaseException as e:
                exc = e
                status = getattr(e, 'code', None)
                raise
            finally:
                try:
                    f = {'method': method, 'path': path, 'dur_ns': _mono() - t0,
                         'status': status, 'socket': getattr(self, 'socket_path', None)}
                    if query:
                        f['query'] = {k: _trunc(v, 120) for k, v in dict(query).items()}
                    if isinstance(data, (bytes, bytearray)):
                        f['req_bytes'] = len(data)
                        if len(data) < 2048 and data[:1] == b'{':
                            try:
                                body = json.loads(data)
                                for key in ('action', 'services', 'checks', 'command'):
                                    if key in body:
                                        v = body[key]
                                        if key == 'command' and isinstance(v, list) and v:
                                            v = os.path.basename(str(v[0]))
                                        f[key] = v
                            except Exception:
                                pass
                    elif data is not None:
                        f['req_stream'] = True
                    if exc is not None:
                        f.update(_exc_fields(exc))
                    _rec('pebble', **f)
                except Exception:
                    pass
        return _request_raw

    def _patch_pebble(mod):
        C = getattr(mod, 'Client', None)
        if C is not None:
            _patch_attr(C, '_request_raw', _make_request_raw, 'pebble.Client._request_raw')

    # ---------------------------------------------------------------- main
    def _make_main(orig):
        def main(*a, **kw):
            _state['main_depth'] += 1
            outer = _state['main_depth'] == 1
            t0 = _mono()
            if outer:
                # Charm init: until ops emits the hook's event, or re-emits deferred ones.
                _state['init'] = _span_open()
                _rec('dispatch_start', sp=_state['init'], **_mem())
            try:
                r = orig(*a, **kw)
                if outer:
                    _state['exit'] = {'rc': 0}
                return r
            except SystemExit as e:
                if outer:
                    code = e.code
                    _state['exit'] = {'rc': code if isinstance(code, int) else (0 if code is None else 1),
                                      'exc': 'SystemExit'}
                raise
            except BaseException as e:
                if outer:
                    _state['exit'] = dict(rc=1, **_exc_fields(e))
                raise
            finally:
                _state['main_depth'] -= 1
                if outer:
                    _state['init'] = None
                    del _spans[1:]
                    f = {'dur_ns': _mono() - t0}
                    f.update(_state['exit'] or {})
                    f.update(_mem())
                    _rec('dispatch_end', **f)
        return main

    def _patch_main(mod):
        m = mod.__dict__.get('main')
        if isinstance(m, types.FunctionType):
            _patch_attr(mod, 'main', _make_main, mod.__name__ + '.main')

    # ---------------------------------------------------------------- import hook
    def _on_ops(mod):
        tracing = sys.modules.get('ops_tracing')
        _rec('ops_loaded', version=getattr(mod, '__version__', None), path=getattr(mod, '__file__', None),
             otel='opentelemetry.trace' in sys.modules, ops_tracing=tracing is not None,
             patched=list(_patched))

    def _sub_and_main(mod):
        _patch_subprocess_user(mod)
        _patch_main(mod)

    _TARGETS = {
        'ops': _on_ops,
        'ops.framework': _patch_framework,
        'ops.pebble': _patch_pebble,
        'ops.model': _patch_subprocess_user,
        'ops.storage': _patch_subprocess_user,
        'ops.hookcmds._utils': _patch_subprocess_user,
        'ops.main': _sub_and_main,
        'ops._main': _sub_and_main,
    }

    def _apply(name, mod):
        fn = _TARGETS.get(name)
        if fn is None:
            return
        try:
            fn(mod)
        except Exception as e:
            _rec('tap_warn', what='patch failed', module=name, **_exc_fields(e))

    class _Loader:
        """Delegating loader: run the real exec_module, then patch the fresh module."""

        def __init__(self, inner, name):
            self._inner = inner
            self._name = name

        def __getattr__(self, attr):
            return getattr(self._inner, attr)

        def create_module(self, spec):
            cm = getattr(self._inner, 'create_module', None)
            return cm(spec) if cm is not None else None

        def exec_module(self, module):
            # Put the real loader back so module.__loader__/__spec__.loader stay unchanged.
            try:
                module.__spec__.loader = self._inner
                module.__loader__ = self._inner
            except Exception:
                pass
            self._inner.exec_module(module)
            _apply(self._name, module)

    class _Finder:
        def find_spec(self, name, path=None, target=None):
            if name not in _TARGETS:
                return None
            for finder in sys.meta_path:
                if finder is self:
                    continue
                fs = getattr(finder, 'find_spec', None)
                if fs is None:
                    continue
                try:
                    spec = fs(name, path, target)
                except Exception:
                    return None
                if spec is not None:
                    if spec.loader is not None and hasattr(spec.loader, 'exec_module'):
                        spec.loader = _Loader(spec.loader, name)
                    return spec
            return None

        def invalidate_caches(self):
            pass

    try:
        _open()
        if 'proc' in _FEATURES:
            import atexit

            _rec('proc_start', pid=os.getpid(), ppid=os.getppid(), uid=os.geteuid(),
                 py=sys.version.split()[0], exe=sys.executable, argv=_cmdline(), cwd=os.getcwd(),
                 features=sorted(_FEATURES),
                 env={k[5:].lower(): os.environ[k] for k in _ENV_KEYS if k in os.environ}, **_mem())
            atexit.register(_at_exit)
        if 'ops' in _FEATURES:
            for _n in list(_TARGETS):
                if _n in sys.modules:  # loaded before us (unusual): patch in place
                    _apply(_n, sys.modules[_n])
            sys.meta_path.insert(0, _Finder())
    except Exception:
        pass
    if _fd >= 0 and 'stacks' in _FEATURES:
        try:
            import atexit

            _sampler = _Stacks()
            # atexit runs handlers last registered first: the last stacks record comes
            # before proc_end.
            atexit.register(_stacks_at_exit, _sampler)
            _sampler.thread.start()
        except Exception:
            pass
