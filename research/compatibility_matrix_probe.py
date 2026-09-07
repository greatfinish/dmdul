"""Run a disposable DM instance; never open or stop an existing instance.

Credentials come from DM_LAB_DB_PASSWORD. Each case needs a fresh directory.
The source DB is stopped normally before offline export, then only this new
instance is restarted for DMP import into a separate schema.
"""
from __future__ import print_function
import argparse
import os
import re
import socket
import subprocess
import time


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--bin', required=True)
    p.add_argument('--root', required=True)
    p.add_argument('--label', required=True)
    p.add_argument('--dmdul', required=True)
    p.add_argument('--port', type=int, default=5540)
    p.add_argument('--page', type=int, default=4)
    p.add_argument('--charset', type=int, default=1)
    p.add_argument('--case-sensitive', type=int, default=0)
    p.add_argument('--check', type=int, default=3)
    p.add_argument('--hash', default='SHA256')
    a = p.parse_args()
    password = os.environ['DM_LAB_DB_PASSWORD']
    if not a.label.isalnum() or not 5500 <= a.port <= 5599:
        raise RuntimeError('use an alphanumeric case name and a dedicated 5500..5599 port')
    with socket.socket() as probe:
        if probe.connect_ex(('127.0.0.1', a.port)) == 0:
            raise RuntimeError('test port is already in use; refusing to use another instance')
    case = os.path.join(os.path.abspath(a.root), a.label)
    if os.path.exists(case):
        raise RuntimeError('refusing to overwrite existing case ' + case)
    os.makedirs(case)
    db = os.path.join(case, a.label)
    env = dict(os.environ, LD_LIBRARY_PATH=a.bin)
    userid = 'SYSDBA/' + password + '@127.0.0.1:' + str(a.port)
    server = [None]

    def run(args, filename, input_text=None, sql_errors=True):
        child = subprocess.Popen(args, cwd=case, env=env, stdin=subprocess.PIPE,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        try:
            raw = child.communicate(None if input_text is None else input_text.encode('utf-8'), timeout=300)[0]
        except subprocess.TimeoutExpired:
            child.kill()
            child.communicate()
            raise RuntimeError(filename + ': test client timed out')
        with open(os.path.join(case, filename), 'wb') as out:
            out.write(raw)
        text = raw.decode('utf-8', 'replace')
        if child.returncode or (sql_errors and ('[-' in text or '\nerror:' in text)):
            raise RuntimeError(filename + ': ' + text[-3000:])
        return text

    def sql(statement, filename):
        return run([os.path.join(a.bin, 'disql'), userid], filename, statement + '\nexit;\n')

    def start():
        log = open(os.path.join(case, 'server.log'), 'ab')
        server[0] = subprocess.Popen([os.path.join(a.bin, 'dmserver'), os.path.join(db, 'dm.ini')],
                                     cwd=case, env=env, stdout=log, stderr=log)
        log.close()
        for i in range(60):
            if server[0].poll() is not None:
                raise RuntimeError('isolated server exited; inspect ' + case + '/server.log')
            try:
                result = sql('select 1;', 'ready.log')
                if '1' in result and 'fail' not in result.lower():
                    return
            except RuntimeError:
                pass
            time.sleep(1)
        raise RuntimeError('isolated server did not become ready')

    def stop():
        if server[0] is not None and server[0].poll() is None:
            sql('shutdown normal;', 'shutdown.log')
            for i in range(60):
                if server[0].poll() is not None:
                    return
                time.sleep(1)
            raise RuntimeError('isolated server did not stop normally')

    init = [os.path.join(a.bin, 'dminit'), 'PATH=' + case, 'DB_NAME=' + a.label,
            'INSTANCE_NAME=' + a.label, 'PORT_NUM=' + str(a.port), 'BUFFER=100', 'LOG_SIZE=256',
            'PAGE_SIZE=' + str(a.page), 'CHARSET=' + str(a.charset),
            'CASE_SENSITIVE=' + str(a.case_sensitive), 'PAGE_CHECK=' + str(a.check),
            'SYSDBA_PWD=' + password, 'SYSAUDITOR_PWD=' + password]
    if a.check == 2:
        init.append('PAGE_HASH_NAME=' + a.hash)
    run(init, 'init.log')
    try:
        start()
        sql('''CREATE SCHEMA MTEST AUTHORIZATION SYSDBA;
/
CREATE TABLE MTEST.PROBE (ID INT PRIMARY KEY, TXT VARCHAR(100), AMT DECIMAL(18,4), DT DATE);
INSERT INTO MTEST.PROBE VALUES(1,'AsciiText',12.3456,DATE '2026-09-06');
INSERT INTO MTEST.PROBE VALUES(2,'mixedCase',NULL,NULL);
INSERT INTO MTEST.PROBE VALUES(3,NULL,-19.1250,DATE '2000-01-02');
COMMIT;
SELECT * FROM MTEST.PROBE ORDER BY ID;
SELECT PAGE(), UNICODE(), CASE_SENSITIVE();
SELECT CHECKPOINT(100);''', 'setup.log')
        stop()
        output = os.path.join(case, 'output')
        commands = ['set system ' + db + '/SYSTEM.DBF;', 'set data_dir ' + db + ';',
                    'set output_dir ' + output + ';', 'set page_check ' + str(a.check) + ';',
                    'set page_hash ' + a.hash + ';', 'bootstrap;', 'check pages SYSTEM.DBF;', 'check pages;',
                    'unload table MTEST.PROBE;', 'set data_format fldr;', 'unload table MTEST.PROBE;',
                    'set data_format dmp;', 'unload table MTEST.PROBE;', 'exit;']
        text = run([a.dmdul], 'offline.log', '\n'.join(commands))
        if text.count('rows exported: 3') != 3 or text.count('rows failed: 0') != 3 or text.count('bad pages total: 0') != 2 or 'status=WARNING' in text or 'SUCCESS_WITH_WARNINGS' in text or 'error:' in text:
            raise RuntimeError('unexpected export/check counters; inspect ' + case + '/offline.log')
        start()
        sql('CREATE SCHEMA MREST AUTHORIZATION SYSDBA;\n/', 'target.log')
        dmp = os.path.join(output, 'MTEST_PROBE.dmp')
        text = run([os.path.join(a.bin, 'dimp'), userid, 'FILE=' + dmp,
                    'REMAP_SCHEMA=MTEST:MREST', 'NOLOGFILE=Y'], 'import.log')
        result = sql('''SELECT COUNT(*) AS MISSING_ROWS FROM (SELECT * FROM MTEST.PROBE MINUS SELECT * FROM MREST.PROBE);
SELECT COUNT(*) AS EXTRA_ROWS FROM (SELECT * FROM MREST.PROBE MINUS SELECT * FROM MTEST.PROBE);
SELECT * FROM MREST.PROBE ORDER BY ID;''', 'compare.log')
        if len(re.findall(r'^1\s+0\s*$', result, re.MULTILINE)) != 2:
            raise RuntimeError('DMP round-trip differs; inspect ' + case + '/compare.log')
        print(a.label + '\n' + result, flush=True)
    finally:
        stop()


if __name__ == '__main__':
    main()
