import importlib.util
import pathlib
import tempfile
import unittest
import os
import subprocess
from concurrent.futures import ThreadPoolExecutor

spec = importlib.util.spec_from_file_location('escalate', pathlib.Path(__file__).parent / 'assets/scripts/escalate.py')
escalate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(escalate)


class EscalationTests(unittest.TestCase):
    def test_unchanged_backoff_cap_and_changed_conditions(self):
        with tempfile.TemporaryDirectory() as root:
            sent = []
            def send(body):
                sent.append(body)
            for now in [0, 5, 10, 15, 29, 30, 10000]:
                escalate.deliver(root, 'human', 'warning', 'head1 blocked', now, 10, 3, send)
            self.assertEqual(len(sent), 3)
            self.assertIn('suppressed', sent[1])
            for recipient, subject, body in [('human', 'warning', 'head2 blocked'), ('human', 'critical', 'head2 blocked'), ('other', 'warning', 'head2 blocked'), ('human', 'warning', 'head1 blocked')]:
                escalate.deliver(root, recipient, subject, body, 10001, 10, 3, send)
            self.assertEqual(len(sent), 7)

    def test_failed_mail_does_not_consume_budget(self):
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaises(RuntimeError):
                escalate.deliver(root, 'human', 'warning', 'body', 0, 10, 3, lambda body: (_ for _ in ()).throw(RuntimeError('mail unavailable')))
            sent = []
            escalate.deliver(root, 'human', 'warning', 'body', 0, 10, 3, sent.append)
            self.assertEqual(sent, ['body'])

    def test_racing_emitters_send_once(self):
        with tempfile.TemporaryDirectory() as root:
            sent = []
            def run(_):
                escalate.deliver(root, 'human', 'warning', 'body', 0, 10, 3, sent.append)
            with ThreadPoolExecutor(max_workers=8) as pool:
                list(pool.map(run, range(20)))
            self.assertEqual(sent, ['body'])

    def test_corrupt_state_fails_loud(self):
        with tempfile.TemporaryDirectory() as root:
            escalate.deliver(root, 'human', 'warning', 'body', 0, 10, 3, lambda body: None)
            next(pathlib.Path(root).glob('*.json')).write_text('{}')
            with self.assertRaises(ValueError):
                escalate.deliver(root, 'human', 'warning', 'body', 100, 10, 3, lambda body: self.fail('corrupt ledger sent mail'))

    def test_shell_entrypoint_dedup_force_and_delivery_failure(self):
        with tempfile.TemporaryDirectory() as root:
            fake = pathlib.Path(root) / 'gc'
            fake.write_text('#!/bin/sh\n[ "$1 $2 $3 $4 $5 $6 $7" = "mail send human -s warning -m body" ] || exit 91\n[ "${FAIL_MAIL:-0}" = 0 ] || exit 92\nprintf "sent\\n" >> "$MAIL_LOG"\n')
            fake.chmod(0o755)
            log = pathlib.Path(root) / 'mail.log'
            env = dict(os.environ, PATH=root + os.pathsep + os.environ['PATH'], MAIL_LOG=str(log), GC_PACK_STATE_DIR=root, GC_ESCALATION_RECIPIENT='human')
            env.pop('GC_ESCALATE_DEDUP_DISABLE', None)
            command = ['bash', str(pathlib.Path(escalate.__file__).with_suffix('.sh')), '--subject', 'warning', '--message', 'body']
            failed = subprocess.run(command, env=dict(env, FAIL_MAIL='1'), capture_output=True)
            self.assertNotEqual(failed.returncode, 0)
            self.assertFalse(log.exists())
            def run(_):
                subprocess.run(command, env=env, check=True, capture_output=True)
            with ThreadPoolExecutor(max_workers=6) as pool:
                list(pool.map(run, range(12)))
            self.assertEqual(log.read_text().splitlines(), ['sent'])
            subprocess.run(command, env=dict(env, GC_ESCALATE_DEDUP_DISABLE='1'), check=True, capture_output=True)
            self.assertEqual(log.read_text().splitlines(), ['sent', 'sent'])


if __name__ == '__main__':
    unittest.main()
