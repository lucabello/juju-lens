#!/usr/bin/env python3
"""Run the tap against a range of ops releases, in a fake Juju environment.

    python3 internal/unit/pytap/opsmatrix/matrix.py [--cache DIR] [--only OPS]... [--keep-spool DIR]

For each row of MATRIX it builds a venv with uv (cached, so later runs are
quick), installs the tap the way juju-lens-probe does (the module and its .pth
in site-packages), and dispatches the fake charm in charm/ through SEQUENCE:
once without the tap and once with it. Then it checks what the tap recorded,
and that the charm behaved the same with and without it. Exit status 1 if any
check fails.

The hook tools are bin/hooktool under each tool's name, and Pebble is
pebble_stub.py on a unix socket. Nothing here talks to Juju.
"""

import argparse
import json
import os
import shutil
import sqlite3
import gzip
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PYTAP = os.path.dirname(HERE)

# (ops release, Python, with ops[tracing]). Oldest supported to latest, plus
# the releases where the internals the tap patches changed: 2.9 added
# collect-status, 2.21 ops tracing, 3.4 moved the hook tools to ops.hookcmds.
MATRIX = [
    ('1.5.5', '3.8', False),
    ('2.0.0', '3.8', False),
    ('2.9.0', '3.10', False),
    ('2.15.0', '3.10', False),
    ('2.20.0', '3.12', False),
    ('2.21.1', '3.12', True),
    ('2.23.6', '3.12', False),
    ('3.0.0', '3.12', True),
    ('3.3.1', '3.12', False),
    ('3.4.0', '3.12', False),
    ('3.9.0', '3.12', True),
    ('3.9.0', '3.14', False),
]

TOOLS = [
    'action-get', 'action-set', 'action-log', 'action-fail', 'application-version-set',
    'config-get', 'goal-state', 'is-leader', 'juju-log', 'network-get', 'opened-ports',
    'relation-get', 'relation-ids', 'relation-list', 'relation-set', 'secret-add',
    'secret-get', 'secret-set', 'state-get', 'state-set', 'status-get', 'status-set',
]

# (dispatch path, extra environment, expected exit status)
SEQUENCE = [
    ('hooks/config-changed', {}, 0),
    ('hooks/db-relation-changed', {'JUJU_RELATION': 'db', 'JUJU_RELATION_ID': '1',
                                   'JUJU_REMOTE_UNIT': 'remote/0', 'JUJU_REMOTE_APP': 'remote',
                                   'FAKE_BUSY': '1'}, 0),
    ('actions/do-thing', {'JUJU_ACTION_NAME': 'do-thing', 'JUJU_ACTION_UUID': '7'}, 0),
    ('hooks/leader-elected', {}, 0),
    ('hooks/start', {'FAKE_RAISE': '1'}, 1),
]


def ver(v):
    return tuple(int(x) for x in v.split('.')[:2])


def row_name(row):
    ops_v, py, tracing = row
    return 'ops-%s%s-py%s' % (ops_v, '-tracing' if tracing else '', py)


def build_venv(cache, row):
    ops_v, py, tracing = row
    venv = os.path.join(cache, row_name(row))
    python = os.path.join(venv, 'bin', 'python')
    if os.path.exists(os.path.join(venv, '.complete')):
        return python
    shutil.rmtree(venv, ignore_errors=True)
    subprocess.run(['uv', 'venv', '-q', '--python', py, venv], check=True)
    reqs = ['ops==' + ops_v]
    if tracing:
        reqs.append('ops-tracing==' + ops_v)
    subprocess.run(['uv', 'pip', 'install', '-q', '--python', python] + reqs, check=True)
    open(os.path.join(venv, '.complete'), 'w').close()
    return python


def site_packages(python):
    out = subprocess.run([python, '-c', 'import sysconfig; print(sysconfig.get_paths()["purelib"])'],
                         check=True, stdout=subprocess.PIPE)
    return out.stdout.decode().strip()


def set_tap(site, on):
    for name in ('_juju_lens_tap.py', '_juju_lens_tap.pth'):
        dst = os.path.join(site, name)
        if on:
            shutil.copy(os.path.join(PYTAP, name), dst)
        elif os.path.exists(dst):
            os.unlink(dst)
    shutil.rmtree(os.path.join(site, '__pycache__'), ignore_errors=True)


def make_spool(path):
    os.makedirs(path)
    os.chmod(path, 0o700)
    with open(os.path.join(path, 'config.json'), 'w') as f:
        json.dump({'features': ['proc', 'ops', 'stacks']}, f)
    open(os.path.join(path, 'alive'), 'w').close()


def run_sequence(python, work, spool, bindir, sock):
    shutil.rmtree(work, ignore_errors=True)
    shutil.copytree(os.path.join(HERE, 'charm'), work)
    results = []
    for i, (dispatch, extra, want_rc) in enumerate(SEQUENCE):
        env = {
            'PATH': bindir + ':/usr/bin:/bin',
            'JUJU_UNIT_NAME': 'fake/0', 'JUJU_MODEL_NAME': 'lab',
            'JUJU_MODEL_UUID': '00000000-0000-4000-8000-000000000001',
            'JUJU_CHARM_DIR': work, 'JUJU_VERSION': '3.6.4',
            'JUJU_DISPATCH_PATH': dispatch, 'JUJU_CONTEXT_ID': 'fake/0-%d' % i,
            'PYTHONPATH': 'lib:src', 'JUJU_LENS_TAP_DIR': spool,
            'FAKE_PEBBLE': sock, 'LANG': 'C.UTF-8',
        }
        env.update(extra)
        p = subprocess.run([python, './src/charm.py'], cwd=work, env=env,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        results.append({'dispatch': dispatch, 'rc': p.returncode, 'want_rc': want_rc,
                        'stdout': p.stdout.decode(errors='replace'),
                        'stderr': p.stderr.decode(errors='replace')})
    return results


def load_spool(spool):
    files = []
    for name in os.listdir(spool):
        if not name.endswith('.jsonl'):
            continue
        with open(os.path.join(spool, name)) as f:
            raw = f.read()
        files.append((raw, [json.loads(line) for line in raw.splitlines() if line.strip()]))
    files.sort(key=lambda f: f[1][0]['t'] if f[1] else 0)
    return files


def span_shapes(work):
    """(name, root?) of every span ops' own tracing buffered, sorted."""
    path = os.path.join(work, '.tracing-data.db')
    if not os.path.exists(path):
        return None
    db = sqlite3.connect(path)
    shapes = []
    for (data,) in db.execute('select data from tracing order by id'):
        if data[:2] == b'\x1f\x8b':
            data = gzip.decompress(data)
        for rs in json.loads(data)['resourceSpans']:
            for ss in rs['scopeSpans']:
                for sp in ss['spans']:
                    shapes.append((sp['name'], not sp.get('parentSpanId')))
    db.close()
    return sorted(shapes)


def strip_tap_frames(text):
    """A traceback through an observer shows the tap's wrapper frame; drop it."""
    out, skip = [], False
    for line in text.splitlines():
        if skip:
            skip = False
            if line.startswith('    '):
                continue
        if '_juju_lens_tap.py' in line:
            skip = True
            continue
        out.append(line)
    return '\n'.join(out)


def checks(row, files, plain, tapped, spans_plain, spans_tapped):
    """A list of (name, ok, detail)."""
    out = []

    def add(name, ok, detail=''):
        out.append((name, bool(ok), detail))

    raw = ''.join(r for r, _ in files)
    add('one file per dispatch', len(files) == len(SEQUENCE), len(files))
    add('no marker value in any record', 'MARKER' not in raw,
        [line[:200] for line in raw.splitlines() if 'MARKER' in line][:3])
    add('exit status as expected, with and without the tap',
        [r['rc'] for r in tapped] == [r['rc'] for r in plain] == [r['want_rc'] for r in plain],
        ([r['rc'] for r in plain], [r['rc'] for r in tapped]))
    add('same stdout with and without the tap',
        [r['stdout'] for r in tapped] == [r['stdout'] for r in plain])
    add('same stderr with and without the tap, but for its frames in tracebacks',
        [strip_tap_frames(r['stderr']) for r in tapped] == [strip_tap_frames(r['stderr']) for r in plain],
        [strip_tap_frames(r['stderr'])[-300:] for r in tapped if r['stderr']][:1])
    add('no tap_warn', not any(r['k'] == 'tap_warn' for _, recs in files for r in recs),
        [r for _, recs in files for r in recs if r['k'] == 'tap_warn'][:2])
    add('every string field at most 300 characters (+ length suffix)',
        all(len(v) <= 320 for _, recs in files for r in recs for k, v in r.items()
            if isinstance(v, str) and k not in ('exe', 'path', 'cwd')))
    if spans_plain is not None or row[2]:
        add('ops tracing exports the same spans with the tap',
            spans_plain is not None and spans_plain == spans_tapped,
            (len(spans_plain or []), len(spans_tapped or [])))
    if len(files) != len(SEQUENCE):
        return out

    d = [recs for _, recs in files]

    def ks(recs, k):
        return [r for r in recs if r['k'] == k]

    add('ops_loaded in every process', all(len(ks(x, 'ops_loaded')) == 1 for x in d))
    add('ops version', ks(d[0], 'ops_loaded')[0]['version'] == row[0], ks(d[0], 'ops_loaded')[0]['version'])
    add('1: config_changed deferred',
        any(r['deferred'] and 'config_changed' in r['ev'] for r in ks(d[0], 'observer_end'))
        and len(ks(d[0], 'defer')) == 1)
    rb = ks(d[1], 'reemit_backlog')
    add('2: backlog of 1', bool(rb) and rb[0]['count'] == 1, rb[0]['count'] if rb else None)
    os2 = ks(d[1], 'observer_start')
    add('2: config_changed re-emitted', any(r['reemit'] and 'config_changed' in r['ev'] for r in os2))
    add('2: custom event emitted inside an observer',
        any(r['cls'] == 'custom' and r['ev_kind'] == 'ready' and r.get('in_observer') for r in ks(d[1], 'emit')))
    add('2: Juju event emitted',
        any(r['cls'] == 'juju' and r['ev_kind'] == 'db_relation_changed' for r in ks(d[1], 'emit')))
    add('2: pre_commit and commit',
        {'pre_commit', 'commit'} <= {r['ev_kind'] for r in ks(d[1], 'emit') if r['cls'] == 'lifecycle'})
    if ver(row[0]) >= (2, 9):
        add('2: collect_unit_status observed', any(r['ev_kind'] == 'collect_unit_status' for r in os2))
    add('2: a direct call to a handler is not an observer call', not any('_reconcile' in r['obs'] for r in os2))
    rs = [r for r in ks(d[1], 'hook_tool') if r['tool'] == 'relation-set']
    add('2: relation-set keys recorded',
        any('password' in (r.get('stdin_keys') or []) or any('password' in a for a in r['args']) for r in rs),
        rs)
    ss = ks(d[1], 'status_set')
    add('2: unit and app status',
        any(r['status'] == 'active' and not r['app'] for r in ss)
        and any(r['app'] and r['message'] == '-app ok' for r in ss),
        [(r['status'], r['message'], r['app']) for r in ss])
    pb = ks(d[1], 'pebble')
    add('2: Pebble calls with their status',
        sorted((r['method'], r['status']) for r in pb)
        == [('GET', 200), ('GET', 404), ('POST', 200), ('POST', 200)],
        [(r['method'], r['path'], r['status']) for r in pb])
    add('3: action emitted', any(r['cls'] == 'action' for r in ks(d[2], 'emit')))
    add('3: action-set recorded', any(r['tool'] == 'action-set' for r in ks(d[2], 'hook_tool')))
    failed = [r for r in ks(d[2], 'hook_tool') if r['tool'] == 'status-set' and r['rc'] != 0]
    add('3: failing hook tool stderr cut to 300 characters',
        failed and 'ERROR invalid status' in failed[0].get('stderr', '')
        and len(failed[0]['stderr']) <= 320, failed[:1])
    add('4: one handler observed for two events',
        any(r['obs'].endswith('_reconcile') for r in ks(d[3], 'observer_start')))
    add('5: observer exception', any(r.get('exc') == 'RuntimeError' for r in ks(d[4], 'observer_end')))
    pe = [ks(x, 'proc_end') for x in d]
    add('proc_end in every process', all(len(p) == 1 for p in pe))
    add('proc_end exit status', [p[0].get('rc') for p in pe if p] == [r['want_rc'] for r in plain],
        [(p[0].get('rc'), p[0].get('exc')) for p in pe if p])
    de = ks(d[4], 'dispatch_end')
    add('5: dispatch_end has the exception, its message cut',
        de and de[0].get('exc') == 'RuntimeError' and len(de[0].get('exc_msg', '')) <= 320, de[:1])

    prof = [stacks_of(x) for x in d]
    add('stacks_end in every process, after the last stacks record and before proc_end',
        all(p['end_at'] is not None and p['end_at'] > p['last_stacks_at'] and p['end_at'] < p['proc_end_at']
            for p in prof))
    add('every sample on the timeline, every kept one in the totals',
        all(p['end'] and len(p['timeline']) == p['end']['samples']
            and sum(t[0] for t in p['totals'].values()) == p['end']['samples'] - p['end']['dropped']
            for p in prof),
        [(len(p['timeline']), p['end'] and p['end']['samples']) for p in prof])
    add('every stack in a span the records opened, or the process (0)',
        all({s for s, _ in p['stacks']} <= {0} | {r['sp'] for r in x if 'sp' in r} for p, x in zip(prof, d)))
    add('no tap frames in stacks',
        not any(f[0].endswith('_juju_lens_tap.py') for p in prof for f in p['frames']))
    init_sp = [r['sp'] for r in ks(d[1], 'dispatch_start')]
    add('2: samples in charm init are in FakeCharm.__init__',
        init_sp and any(sp == init_sp[0] and {'_spin', '__init__'} <= prof[1]['funcs'](fr)
                        for sp, fr in prof[1]['stacks']),
        [(sp, sorted(prof[1]['funcs'](fr))) for sp, fr in prof[1]['stacks']][:6])
    obs_sp = [r['sp'] for r in ks(d[1], 'observer_start') if r['obs'].endswith('_on_db_changed')]
    add('2: samples in the observer are in _on_db_changed',
        obs_sp and any(sp == obs_sp[0] and {'_spin', '_on_db_changed'} <= prof[1]['funcs'](fr)
                       for sp, fr in prof[1]['stacks']))
    return out


def stacks_of(recs):
    """A process's stacks records put back together."""
    p = {'frames': [], 'stacks': [], 'totals': {}, 'timeline': [], 'end': None, 'end_at': None,
         'last_stacks_at': -1, 'proc_end_at': len(recs)}
    for i, r in enumerate(recs):
        if r['k'] == 'stacks':
            p['last_stacks_at'] = i
            p['frames'][r['f0']:] = r['frames']
            p['stacks'][r['s0']:] = r['stacks']
            for sid, n, wall, cpu, mem in r['totals']:
                t = p['totals'].setdefault(sid, [0, 0, 0, 0])
                for j, v in enumerate((n, wall, cpu, mem)):
                    t[j] += v
            tl = r['tl']
            p['timeline'] += [tl[j:j + 3] for j in range(0, len(tl), 3)]
        elif r['k'] == 'stacks_end':
            p['end'], p['end_at'] = r, i
        elif r['k'] == 'proc_end':
            p['proc_end_at'] = i
    p['funcs'] = lambda fr: {p['frames'][f][1].rsplit('.', 1)[-1] for f in fr}
    return p


def start_pebble(sock):
    p = subprocess.Popen([sys.executable, os.path.join(HERE, 'pebble_stub.py'), sock])
    for _ in range(200):
        if os.path.exists(sock):
            return p
        time.sleep(0.01)
    p.kill()
    raise RuntimeError('Pebble stub did not start')


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    ap.add_argument('--cache', default=os.path.join(
        os.environ.get('XDG_CACHE_HOME') or os.path.expanduser('~/.cache'), 'juju-lens', 'opsmatrix'),
        help='where the venvs are kept between runs')
    ap.add_argument('--only', action='append', metavar='OPS',
                    help='run only the rows for this ops release (repeatable)')
    ap.add_argument('-v', '--verbose', action='store_true', help='print every check, not only failures')
    ap.add_argument('--keep-spool', metavar='DIR',
                    help="copy each row's spool files, as the tap wrote them, to DIR/<row>/")
    args = ap.parse_args()

    rows = [r for r in MATRIX if not args.only or r[0] in args.only]
    if not rows:
        ap.error('no matrix row for %s' % ', '.join(args.only))
    os.makedirs(args.cache, exist_ok=True)
    tmp = tempfile.mkdtemp(prefix='juju-lens-opsmatrix-')
    bindir = os.path.join(tmp, 'bin')
    os.makedirs(bindir)
    for t in TOOLS:
        os.symlink(os.path.join(HERE, 'bin', 'hooktool'), os.path.join(bindir, t))
    pebble = start_pebble(os.path.join(tmp, 'pebble.sock'))
    failed = 0
    try:
        for row in rows:
            name = row_name(row)
            python = build_venv(args.cache, row)
            site = site_packages(python)
            set_tap(site, False)
            plain_work = os.path.join(tmp, name, 'plain')
            plain = run_sequence(python, plain_work, os.path.join(tmp, name, 'no-spool'), bindir,
                                 os.path.join(tmp, 'pebble.sock'))
            spool = os.path.join(tmp, name, 'spool')
            make_spool(spool)
            set_tap(site, True)
            tapped_work = os.path.join(tmp, name, 'tapped')
            try:
                tapped = run_sequence(python, tapped_work, spool, bindir, os.path.join(tmp, 'pebble.sock'))
            finally:
                set_tap(site, False)
            if args.keep_spool:
                shutil.copytree(spool, os.path.join(args.keep_spool, name), dirs_exist_ok=True)
            results = checks(row, load_spool(spool), plain, tapped,
                             span_shapes(plain_work), span_shapes(tapped_work))
            bad = [c for c in results if not c[1]]
            failed += len(bad)
            print('%-28s %d/%d checks passed' % (name, len(results) - len(bad), len(results)))
            for check, ok, detail in results:
                if args.verbose or not ok:
                    print('  [%s] %s%s' % ('ok' if ok else 'FAIL', check, '  %s' % (detail,) if detail != '' else ''))
    finally:
        pebble.terminate()
        shutil.rmtree(tmp, ignore_errors=True)
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
