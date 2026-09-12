#!/usr/bin/env python3
"""Bound unchanged core escalation mail; preserve every condition transition."""

import fcntl
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import time


def deliver(root, recipient, subject, body, now, interval, cap, send):
    """Serialize one subject's cadence, recording only successfully sent mail."""
    os.makedirs(root, mode=0o700, exist_ok=True)
    key = hashlib.sha256(json.dumps([recipient, subject]).encode()).hexdigest()
    fingerprint = hashlib.sha256(body.encode()).hexdigest()
    path = os.path.join(root, key + '.json')
    # Never unlink lock files: concurrent openers must share the same inode.
    with open(os.path.join(root, key + '.lock'), 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            with open(path) as saved:
                state = json.load(saved)
            if (not isinstance(state, dict)
                    or not isinstance(state.get('fingerprint'), str)
                    or any(type(state.get(k)) is not int or state[k] < 0 for k in ('sent', 'last_sent', 'suppressed'))):
                raise ValueError('corrupt escalation receipt: ' + path)
        except FileNotFoundError:
            state = None
        if state is None or state['fingerprint'] != fingerprint:
            state = dict(fingerprint=fingerprint, sent=0, last_sent=0, suppressed=0)
        count = state['sent']
        quiet = count >= cap or (count > 0 and now - state['last_sent'] < min(86400, interval * 2 ** min(count - 1, 20)))
        if quiet:
            state['suppressed'] += 1
        else:
            message = body
            if state['suppressed']:
                message += '\n\n[escalate] suppressed %d unchanged occurrence(s).' % state['suppressed']
            send(message)
            state.update(sent=count + 1, last_sent=now, suppressed=0)
        fd, pending = tempfile.mkstemp(prefix=key + '.', dir=root)
        try:
            with os.fdopen(fd, 'w') as output:
                json.dump(state, output)
                output.flush()
                os.fsync(output.fileno())
            os.replace(pending, path)
            directory = os.open(root, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(pending):
                os.unlink(pending)


def main():
    recipient, subject, body = sys.argv[1:]
    def send(message):
        subprocess.run(['gc', 'mail', 'send', recipient, '-s', subject, '-m', message], check=True)
    # Explicit incident/operator notification bypass remains available.
    if os.environ.get('GC_ESCALATE_DEDUP_DISABLE') == '1':
        send(body)
        return
    interval = int(os.environ.get('GC_ESCALATE_REPEAT_SECONDS', '3600'))
    cap = int(os.environ.get('GC_ESCALATE_MAX_SENDS', '5'))
    if interval <= 0 or cap <= 0:
        raise ValueError('escalation repeat interval and send cap must be positive')
    city = os.environ.get('GC_CITY_PATH') or os.environ.get('GC_CITY', '.')
    root = os.path.join(os.environ.get('GC_PACK_STATE_DIR', os.path.join(city, '.gc/runtime/packs/core')), 'escalations')
    deliver(root, recipient, subject, body, int(time.time()), interval, cap, send)


if __name__ == '__main__':
    main()
