#!/usr/bin/env python3
"""Convert strace-style escaped strings (\0\0\0\5, \n, \377, ...) to raw bytes.

Usage: oct2bin.py < input.txt > output.bin
       oct2bin.py input.txt output.bin
"""
import re
import sys

ESCAPES = {"n": b"\n", "t": b"\t", "r": b"\r", "a": b"\a", "b": b"\b",
           "f": b"\f", "v": b"\v", "0": b"\0", "\\": b"\\", '"': b'"', "'": b"'"}

TOKEN = re.compile(r"\\([0-7]{1,3}|x[0-9a-fA-F]{2}|.)|([^\\]+)", re.S)


def unescape(text):
    out = bytearray()
    for esc, plain in TOKEN.findall(text):
        if plain:
            out += plain.encode("latin-1")
        elif esc.startswith("x"):
            out.append(int(esc[1:], 16))
        elif esc.isdigit():
            out.append(int(esc, 8) & 0xFF)
        else:
            out += ESCAPES.get(esc, esc.encode("latin-1"))
    return bytes(out)


def main():
    args = sys.argv[1:]
    text = open(args[0], encoding="latin-1").read() if args else sys.stdin.read()
    text = text.strip().strip('"')
    data = unescape(text)
    if len(args) > 1:
        open(args[1], "wb").write(data)
    else:
        sys.stdout.buffer.write(data)


if __name__ == "__main__":
    main()
