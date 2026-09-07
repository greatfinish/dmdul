from __future__ import print_function
import binascii
import glob
import os
import struct

ROOT = '/dmdata/dmdulundolab/snapshots/'

for folder in sorted(glob.glob(ROOT + '*')):
    print('\nSNAPSHOT', os.path.basename(folder))
    with open(folder + '/ROLL.DBF', 'rb') as f:
        p = 0
        while True:
            raw = f.read(8192)
            if len(raw) != 8192:
                break
            if raw[20:24] == b'\x1d\0\0\0':
                trx = int(binascii.hexlify(raw[36:42][::-1]), 16)
                if 6775 <= trx <= 6790:
                    print('ROLL', p, 'trx', trx, 'header', binascii.hexlify(raw[:64]))
            p += 1

for before, after in [('01_insert_open','02_insert_commit'), ('04_update_two','05_delete_open'), ('05_delete_open','06_rollback')]:
    print('\nDIFF', before, after)
    for name in ['ROLL.DBF','SYSTEM.DBF']:
        with open(ROOT+before+'/'+name,'rb') as a, open(ROOT+after+'/'+name,'rb') as b:
            p = 0
            while True:
                x,y=a.read(8192),b.read(8192)
                if len(x)!=8192 or len(y)!=8192:
                    break
                if x!=y:
                    pos=[i for i in range(8192) if x[i]!=y[i]]
                    print(name,p,'kind',struct.unpack_from('<I',x,20)[0], 'changes',len(pos))
                    for start in pos[:100]:
                        print(hex(start),binascii.hexlify(x[start:start+1]),binascii.hexlify(y[start:start+1]))
                p+=1
