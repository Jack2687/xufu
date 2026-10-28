"""Isolated browser QA. Never seeds or changes the user's local application data."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
from urllib.parse import urlsplit, urlunsplit, parse_qsl, urlencode
from urllib.request import urlopen
import psycopg2
from psycopg2 import sql

root = Path(__file__).resolve().parent.parent
env = os.environ.copy()
for line in (root / '.env').read_text(encoding='utf-8-sig').splitlines():
    if '=' in line:
        k, v = line.split('=', 1)
        env[k] = v
schema = 'xupay_browser_' + secrets.token_hex(8)
database = psycopg2.connect(env['DATABASE_URL'])
database.autocommit = True
with database.cursor() as c:
    c.execute(sql.SQL('CREATE SCHEMA {}').format(sql.Identifier(schema)))
parts = urlsplit(env['DATABASE_URL'])
query = dict(parse_qsl(parts.query))
query['search_path'] = schema
env['DATABASE_URL'] = urlunsplit(parts._replace(query=urlencode(query)))
env['PORT'] = '18863'
env['BOOTSTRAP_EMAIL'] = 'qa-admin@xupay.local'
env['BOOTSTRAP_PASSWORD'] = secrets.token_urlsafe(24)
context_file = root / '.local/browser-context.json'
context_file.write_text(json.dumps({'base': 'http://127.0.0.1:18863', 'email': env['BOOTSTRAP_EMAIL'], 'password': env['BOOTSTRAP_PASSWORD']}), encoding='utf-8')
log = (root / '.local/browser-server.log').open('wb')
process = None
try:
    executable = Path(env.get('XUPAY_QA_EXECUTABLE', str(root / '.local/xupay-server.exe')))
    process = subprocess.Popen([str(executable)], cwd=root, env=env, stdout=log, stderr=log, creationflags=subprocess.CREATE_NO_WINDOW)
    for _ in range(60):
        if process.poll() is not None:
            raise RuntimeError('QA server exited')
        try:
            with urlopen('http://127.0.0.1:18863/health/ready', timeout=1) as r:
                if r.status == 200:
                    break
        except Exception:
            time.sleep(.2)
    else:
        raise RuntimeError('QA server readiness timeout')
    result = subprocess.run(['node', str(root / 'scripts/browser-test.mjs')], cwd=root, env=env)
    if result.returncode:
        raise SystemExit(result.returncode)
finally:
    if process and process.poll() is None:
        process.terminate()
        process.wait(timeout=15)
    log.close()
    context_file.unlink(missing_ok=True)
    with database.cursor() as c:
        c.execute(sql.SQL('DROP SCHEMA {} CASCADE').format(sql.Identifier(schema)))
    database.close()
