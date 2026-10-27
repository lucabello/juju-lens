#!/usr/bin/env python3
"""Fake charm for the tap's ops version matrix; runs on ops 1.5 to 3.x.

Every value that must never reach a tap record contains MARKER: config and
databag values, secret contents, action results, Pebble file contents and
layer commands, log messages. The matrix fails if MARKER appears in any
record.
"""

import logging
import os
import time

import ops
import ops.charm
import ops.framework
import ops.main
import ops.model
import ops.pebble

logger = logging.getLogger(__name__)


def _spin(seconds):
    """Busy work, so the stack sampler sees this frame (FAKE_BUSY)."""
    end = time.monotonic() + seconds
    n = 0
    while time.monotonic() < end:
        n += 1
    return n


class Ready(ops.framework.EventBase):
    """Custom event."""


class FakeEvents(ops.charm.CharmEvents):
    ready = ops.framework.EventSource(Ready)


class FakeCharm(ops.charm.CharmBase):
    on = FakeEvents()
    _stored = ops.framework.StoredState()

    def __init__(self, *args):
        super().__init__(*args)
        if os.environ.get('FAKE_BUSY'):
            _spin(0.1)
        try:
            import ops_tracing  # ops[tracing]: the charm sets up its own tracing
        except ImportError:
            pass
        else:
            self.tracing = ops_tracing.Tracing(self, 'charm-tracing')
        self._stored.set_default(defers=0)
        fw = self.framework
        fw.observe(self.on.config_changed, self._on_config_changed)
        fw.observe(self.on.ready, self._on_ready)
        fw.observe(self.on.db_relation_changed, self._on_db_changed)
        fw.observe(self.on.start, self._on_start)
        fw.observe(self.on.do_thing_action, self._on_do_thing)
        # Same handler observed twice: re-observing must still work under the tap.
        fw.observe(self.on.leader_elected, self._reconcile)
        fw.observe(self.on.upgrade_charm, self._reconcile)
        if hasattr(self.on, 'collect_unit_status'):
            fw.observe(self.on.collect_unit_status, self._on_collect_status)

    def _on_config_changed(self, event):
        if self._stored.defers == 0:
            self._stored.defers += 1
            event.defer()
            return
        _ = self.config.get('password')
        logger.info('configured with MARKER-log-message')
        self.on.ready.emit()
        self._reconcile(event)  # direct call by charm code: not an observer call

    def _reconcile(self, event):
        self.unit.status = ops.model.ActiveStatus('configured')

    def _on_ready(self, event):
        if self.unit.is_leader():
            for rel in self.model.relations['db']:
                rel.data[self.unit]['password'] = 'MARKER-databag-value'
                rel.data[self.unit]['host'] = '10.1.2.3'
            self.app.status = ops.model.ActiveStatus('-app ok')  # leading '-' after '--'
        sock = os.environ.get('FAKE_PEBBLE')
        if sock:
            client = ops.pebble.Client(socket_path=sock)
            client.get_system_info()
            try:
                client.get_services(names=['missing'])
            except ops.pebble.APIError:
                pass
            client.push('/etc/fake.conf', 'token = MARKER-pebble-file\n')
            client.add_layer('fake', {
                'services': {'fake': {'override': 'replace', 'command': 'fake --token MARKER-layer'}},
            }, combine=True)

    def _on_db_changed(self, event):
        _ = event.relation.data[event.unit].get('host')
        if os.environ.get('FAKE_BUSY'):
            _spin(0.1)

    def _on_start(self, event):
        if os.environ.get('FAKE_RAISE'):
            raise RuntimeError('boom from start handler ' + 'x' * 1000)

    def _on_do_thing(self, event):
        if hasattr(self.app, 'add_secret'):
            self.app.add_secret({'apikey': 'MARKER-secret-content'}, label='lab')
        event.set_results({'token': 'MARKER-action-result', 'n': 3})
        try:
            # The fake status-set fails on "boom", with a long error message.
            self.unit.status = ops.model.MaintenanceStatus('boom')
        except ops.model.ModelError:
            pass

    def _on_collect_status(self, event):
        event.add_status(ops.model.ActiveStatus('collected'))


if __name__ == '__main__':
    main = ops.main if callable(ops.main) else ops.main.main
    main(FakeCharm)
