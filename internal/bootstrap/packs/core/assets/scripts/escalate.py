#!/usr/bin/env python3
"""Bound unchanged core escalation mail; preserve every condition transition."""

import fcntl
import hashlib
import json
import os
import signal
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


def send_timeout_secs():
    """Return the positive send bound escalate.sh exported (default 30s)."""
    raw = os.environ.get('GC_ESCALATE_SEND_TIMEOUT_SECS', '30')
    return int(raw) if raw.isdigit() and int(raw) > 0 else 30


def send_mail(recipient, subject, message, notify, limit):
    """Send one mail, bounding the send plus its wake by limit seconds.

    The mail bead is written before the wake blocks, so a bound that trips
    costs the wake and not the message: it is reported and counted as sent,
    which keeps a caller from retrying mail that landed. The child runs in its
    own session so the whole wake process group is reaped on expiry.
    """
    argv = ['gc', 'mail', 'send', recipient]
    if notify:
        argv.append('--notify')
    argv += ['-s', subject, '-m', message]
    proc = subprocess.Popen(argv, start_new_session=True)
    try:
        rc = proc.wait(timeout=limit)
    except subprocess.TimeoutExpired:
        for sig, grace in ((signal.SIGTERM, 2), (signal.SIGKILL, None)):
            try:
                os.killpg(proc.pid, sig)
            except ProcessLookupError:
                break
            try:
                proc.wait(timeout=grace)
                break
            except subprocess.TimeoutExpired:
                continue
        print('escalate: mail to %s sent; wake exceeded %ds and was abandoned' % (recipient, limit), file=sys.stderr)
        return
    if rc != 0:
        raise subprocess.CalledProcessError(rc, argv)


def main():
    recipient, subject, body = sys.argv[1:]
    notify = os.environ.get('GC_ESCALATE_NOTIFY') == '1'
    limit = send_timeout_secs()
    def send(message):
        send_mail(recipient, subject, message, notify, limit)
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
