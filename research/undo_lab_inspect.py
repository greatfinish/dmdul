"""Print row tails and referenced ROLL bytes from the disposable capture."""
from __future__ import print_function
import binascii
import glob
import os
import struct


def num(raw):
    return int(binascii.hexlify(raw[::-1]), 16)


for folder in sorted(glob.glob('/dmdata/dmdulundolab/snapshots/*')):
    print('\nSNAPSHOT', os.path.basename(folder))
    with open(folder + '/ROLL.DBF', 'rb') as roll, open(folder + '/MAIN.DBF', 'rb') as main:
        page_no = 0
        while True:
            page = main.read(8192)
            if len(page) != 8192:
                break
            if page[20:24] == b'\x14\0\0\0' and any(x in page for x in [b'BASE_', b'UPDATE', b'UNCOMMITTED']):
                print('PAGE', page_no, 'HEADER', binascii.hexlify(page[:98]))
                off = 98
                while off + 3 < 8192:
                    word = struct.unpack_from('>H', page, off)[0]
                    length = word & 0x7fff
                    if length < 22 or off + length > 8192:
                        break
                    row = page[off:off+length]
                    tail = row[-19:]
                    print('ROW', off, length, 'deleted', bool(word & 0x8000), 'trx', num(tail[13:19]), 'roll', num(tail[6:7]), num(tail[7:11]), num(tail[11:13]), 'hex', binascii.hexlify(row))
                    rp, ro = num(tail[7:11]), num(tail[11:13])
                    if rp < 16000 and ro < 8192:
                        roll.seek(rp*8192)
                        undo = roll.read(8192)
                        print('ROLLHEADER', binascii.hexlify(undo[:106]))
                        print('ROLLAT', ro, binascii.hexlify(undo[max(0,ro-16):min(8192,ro+256)]))
                    off += length
            page_no += 1
