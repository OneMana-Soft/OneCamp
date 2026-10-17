package zipsafe

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Open is how every zip someone gives the server is opened: business/ContentIO's
// imports, the zips in them and the Word files they hold; business/SlackImport's
// exports; the AI's reading of Office files.
//
// archive/zip's NewReader reads a zip's whole central directory before
// anything can count it. It makes a File for every record, with copies of the
// record's name, extra field and comment, and it reads records past the count
// the zip claims, comparing only the low 16 bits. Measured 16 Oct 2026: a
// 90 MiB zip of a million empty entries took 1 s and 222 MiB to open, before
// the import's own limit of 50,000 files was asked (ValidateZip, and
// ContentIO's count, both come after it). A zip inside a zip, unpacked to
// disk within an import's budget, can hold ten times that.
//
// Open reads the directory first, the way archive/zip will, and keeps
// nothing: the end record, then each record's fixed part, skipping the rest.
// It refuses a zip past MaxEntries or MaxDirectoryBytes before NewReader
// allocates anything. Its other limits are ValidateZip's, for the caller to
// ask.

// ErrDirectoryTooLarge is a zip whose central directory takes more bytes
// than Limits.MaxDirectoryBytes.
var ErrDirectoryTooLarge = errors.New("zip central directory exceeds its size cap")

// Zip format constants, as archive/zip has them.
const (
	directoryHeaderSignature = 0x02014b50
	directory64LocSignature  = 0x07064b50
	directory64EndSignature  = 0x06064b50
	directoryHeaderLen       = 46
	directoryEndLen          = 22
	directory64LocLen        = 20
	directory64EndLen        = 56
)

// Open opens a zip of size bytes within l's MaxEntries and
// MaxDirectoryBytes (0 for no cap): an error wrapping ErrTooManyEntries or
// ErrDirectoryTooLarge past them, before archive/zip reads the directory;
// else zip.NewReader's answer.
func Open(r io.ReaderAt, size int64, l Limits) (*zip.Reader, error) {
	if size < 0 {
		return nil, errors.New("zip: size cannot be negative")
	}
	starts, claimed, err := directoryStarts(r, size)
	if err != nil {
		return nil, err
	}
	if l.MaxEntries > 0 && claimed > uint64(l.MaxEntries) {
		return nil, fmt.Errorf("%w: claims %d, max %d", ErrTooManyEntries, claimed, l.MaxEntries)
	}
	for _, at := range starts {
		if err := directoryWithin(r, size, at, l); err != nil {
			return nil, err
		}
	}
	return zip.NewReader(r, size)
}

// directoryWithin reads the records from at as archive/zip does (until one
// whose fixed part or signature doesn't read), counting them and their
// bytes, and stops at the first limit passed.
func directoryWithin(r io.ReaderAt, size, at int64, l Limits) error {
	if at < 0 || at >= size {
		return nil
	}
	buf := bufio.NewReader(io.NewSectionReader(r, at, size-at))
	var head [directoryHeaderLen]byte
	entries, total := 0, int64(0)
	for {
		if _, err := io.ReadFull(buf, head[:]); err != nil {
			return nil
		}
		if binary.LittleEndian.Uint32(head[:4]) != directoryHeaderSignature {
			return nil
		}
		rest := int(binary.LittleEndian.Uint16(head[28:30])) + int(binary.LittleEndian.Uint16(head[30:32])) +
			int(binary.LittleEndian.Uint16(head[32:34]))
		entries++
		total += directoryHeaderLen + int64(rest)
		if l.MaxEntries > 0 && entries > l.MaxEntries {
			return fmt.Errorf("%w: have more than %d", ErrTooManyEntries, l.MaxEntries)
		}
		if l.MaxDirectoryBytes > 0 && total > l.MaxDirectoryBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrDirectoryTooLarge, l.MaxDirectoryBytes)
		}
		if _, err := buf.Discard(rest); err != nil {
			return nil
		}
	}
}

// directoryStarts is where archive/zip may start reading the directory, as
// its readDirectoryEnd finds it (the end record in the last 1 KiB or 65 KiB,
// the zip64 end record it points to, the base offset), with the count of
// records the zip claims. With a base offset other than 0, both starts it
// chooses between: which one it takes depends on whether a record reads at
// the second.
func directoryStarts(r io.ReaderAt, size int64) (starts []int64, claimed uint64, err error) {
	var block []byte
	var endOffset int64
	found := false
	for i, n := range []int64{1024, 65 * 1024} {
		if n > size {
			n = size
		}
		block = make([]byte, int(n))
		if _, err := r.ReadAt(block, size-n); err != nil && err != io.EOF {
			return nil, 0, err
		}
		if p := endInBlock(block); p >= 0 {
			block, endOffset, found = block[p:], size-n+int64(p), true
			break
		}
		if i == 1 || n == size {
			break
		}
	}
	if !found {
		return nil, 0, zip.ErrFormat
	}
	b := block[4:]
	records := uint64(binary.LittleEndian.Uint16(b[6:8]))
	dirSize := uint64(binary.LittleEndian.Uint32(b[8:12]))
	dirOffset := uint64(binary.LittleEndian.Uint32(b[12:16]))
	if records == 0xffff || dirSize == 0xffff || dirOffset == 0xffffffff {
		p, err := directory64End(r, endOffset)
		if err == nil && p >= 0 {
			endOffset = p
			records, dirSize, dirOffset, err = directory64Values(r, p)
		}
		if err != nil {
			return nil, 0, err
		}
	}
	const maxInt64 = uint64(1<<63 - 1)
	if dirSize > maxInt64 || dirOffset > maxInt64 {
		return nil, 0, zip.ErrFormat
	}
	base := endOffset - int64(dirSize) - int64(dirOffset)
	if o := base + int64(dirOffset); o < 0 || o >= size {
		return nil, 0, zip.ErrFormat
	}
	starts = []int64{base + int64(dirOffset)}
	if base > 0 {
		starts = append(starts, int64(dirOffset))
	}
	return starts, records, nil
}

// endInBlock is where the end record's signature is in b, as archive/zip
// finds it (the last one whose comment fits), or -1.
func endInBlock(b []byte) int {
	for i := len(b) - directoryEndLen; i >= 0; i-- {
		if b[i] == 'P' && b[i+1] == 'K' && b[i+2] == 0x05 && b[i+3] == 0x06 {
			n := int(b[i+directoryEndLen-2]) | int(b[i+directoryEndLen-1])<<8
			if n+directoryEndLen+i > len(b) {
				return -1
			}
			return i
		}
	}
	return -1
}

// directory64End is where the zip64 end record is, from the locator just
// before the end record; -1 for none.
func directory64End(r io.ReaderAt, endOffset int64) (int64, error) {
	at := endOffset - directory64LocLen
	if at < 0 {
		return -1, nil
	}
	b := make([]byte, directory64LocLen)
	if _, err := r.ReadAt(b, at); err != nil {
		return -1, err
	}
	if binary.LittleEndian.Uint32(b[0:4]) != directory64LocSignature || binary.LittleEndian.Uint32(b[4:8]) != 0 ||
		binary.LittleEndian.Uint32(b[16:20]) != 1 {
		return -1, nil
	}
	return int64(binary.LittleEndian.Uint64(b[8:16])), nil
}

// directory64Values is the zip64 end record's count of records, and the
// directory's size and offset.
func directory64Values(r io.ReaderAt, at int64) (records, size, offset uint64, err error) {
	b := make([]byte, directory64EndLen)
	if _, err := r.ReadAt(b, at); err != nil {
		return 0, 0, 0, err
	}
	if binary.LittleEndian.Uint32(b[0:4]) != directory64EndSignature {
		return 0, 0, 0, zip.ErrFormat
	}
	return binary.LittleEndian.Uint64(b[32:40]), binary.LittleEndian.Uint64(b[40:48]), binary.LittleEndian.Uint64(b[48:56]), nil
}
