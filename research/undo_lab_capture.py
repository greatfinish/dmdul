"""Capture checkpointed, paused-process DM transaction experiments, not cold backups.

Run only on the disposable UNDOLAB instance at localhost:5439. Credentials
come from DM_UNDO_USERID. No production instance is stopped or modified.
Compatible with Python 2.7/3 (stdlib only).
"""
from __future__ import print_function
import glob
import os
import pty
import select
import shutil
import signal
import subprocess
import sys
import termios
import time

BASE = '/dmdata/dmdulundolab'
DB = BASE + '/UNDOLAB'


class Session(object):
    def __init__(self, userid):
        self.master, slave = pty.openpty()
        attr = termios.tcgetattr(slave)
        attr[3] &= ~termios.ECHO
        termios.tcsetattr(slave, termios.TCSANOW, attr)
        env = dict(os.environ, LD_LIBRARY_PATH='/dm8/bin')
        self.proc = subprocess.Popen(['/dm8/bin/disql', userid], stdin=slave,
                                     stdout=slave, stderr=slave, env=env)
        os.close(slave)
        self.read_prompt()

    def read_prompt(self):
        data = b''
        end = time.time() + 60
        while time.time() < end:
            if select.select([self.master], [], [], 1)[0]:
                data += os.read(self.master, 65536)
                if data.rstrip().endswith(b'SQL>'):
                    return data.decode('utf-8', 'replace')
        raise RuntimeError('disql timeout: ' + repr(data[-2048:]))

    def sql(self, sql):
        os.write(self.master, (sql + '\n').encode('utf-8'))
        result = self.read_prompt()
        print(sql + '\n' + result)
        sys.stdout.flush()
        if '[-' in result:
            raise RuntimeError(result)
        return result

    def close(self):
        try:
            os.write(self.master, b'exit\n')
            self.proc.wait()
        finally:
            os.close(self.master)


def main():
    userid = os.environ['DM_UNDO_USERID']
    if not userid.endswith('@127.0.0.1:5439'):
        raise RuntimeError('only the disposable localhost:5439 instance is allowed')
    pids = []
    for f in glob.glob('/proc/[0-9]*/cmdline'):
        with open(f, 'rb') as src:
            argv = src.read().split(b'\0')
        if argv[0] == b'/dm8/bin/dmserver' and (DB + '/dm.ini').encode('ascii') in argv:
            pids.append(int(f.split('/')[2]))
    if len(pids) != 1:
        raise RuntimeError('expected exactly one UNDOLAB process: ' + repr(pids))
    root = BASE + '/snapshots'
    if os.path.exists(root):
        raise RuntimeError('snapshots already exist; refusing overwrite')
    os.mkdir(root)
    a, b = Session(userid), Session(userid)

    def snapshot(name):
        b.sql('SELECT CHECKPOINT(100);')
        state = b.sql('SELECT * FROM V$TRX;')
        own = a.sql('SELECT * FROM SYSDBA.UNDO_PROBE ORDER BY ID;')
        visible = b.sql('SELECT * FROM SYSDBA.UNDO_PROBE ORDER BY ID;')
        target = root + '/' + name
        os.mkdir(target)
        with open(target + '/observations.txt', 'w') as f:
            f.write('Checkpointed paused-process sample, NOT a clean shutdown backup.\n')
            f.write(state + '\nOWN SESSION:\n' + own + '\nOTHER SESSION:\n' + visible)
        os.kill(pids[0], signal.SIGSTOP)
        try:
            for name in ['SYSTEM.DBF', 'MAIN.DBF', 'ROLL.DBF', 'dm.ctl', 'dm.ini']:
                shutil.copyfile(DB + '/' + name, target + '/' + name)
        finally:
            os.kill(pids[0], signal.SIGCONT)
        print('SNAPSHOT ' + target)
        sys.stdout.flush()

    try:
        a.sql('CREATE TABLE SYSDBA.UNDO_PROBE (ID INT PRIMARY KEY, VALUE_TX VARCHAR(100));')
        a.sql("INSERT INTO SYSDBA.UNDO_PROBE VALUES (1,'BASE_ONE'),(2,'BASE_TWO'),(3,'BASE_THREE');")
        a.sql('COMMIT;')
        a.sql('SET AUTOCOMMIT OFF;')
        snapshot('00_baseline')
        a.sql("INSERT INTO SYSDBA.UNDO_PROBE VALUES (99,'UNCOMMITTED_INSERT');")
        snapshot('01_insert_open')
        a.sql('COMMIT;')
        snapshot('02_insert_commit')
        a.sql("UPDATE SYSDBA.UNDO_PROBE SET VALUE_TX='FIRST_UPDATE' WHERE ID=1;")
        snapshot('03_update_one')
        a.sql("UPDATE SYSDBA.UNDO_PROBE SET VALUE_TX='SECOND_LONGER_UPDATE' WHERE ID=1;")
        snapshot('04_update_two')
        a.sql('DELETE FROM SYSDBA.UNDO_PROBE WHERE ID=2;')
        snapshot('05_delete_open')
        a.sql('ROLLBACK;')
        snapshot('06_rollback')
        a.sql('DELETE FROM SYSDBA.UNDO_PROBE WHERE ID=3;')
        a.sql('COMMIT;')
        snapshot('07_delete_commit')
    finally:
        a.close()
        b.close()


if __name__ == '__main__':
    main()
