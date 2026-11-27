package imgengine

import (
	"encoding/binary"
	"errors"
	"testing"
)

// tiffWithIFD returns a little-endian TIFF header whose first IFD holds the
// given (tag, LONG value) entries.
func tiffWithIFD(entries [][2]uint32) []byte {
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	b = binary.LittleEndian.AppendUint16(b, uint16(len(entries)))
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, uint16(e[0]))
		b = binary.LittleEndian.AppendUint16(b, 4) // LONG
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint32(b, e[1])
	}
	return binary.LittleEndian.AppendUint32(b, 0) // no next IFD
}

// heicPrefix returns an ftyp box branded heic, optionally followed by an ispe
// box declaring w×h.
func heicPrefix(withISPE bool, w, h uint32) []byte {
	b := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0}
	if withISPE {
		b = append(b, 0, 0, 0, 20, 'i', 's', 'p', 'e', 0, 0, 0, 0)
		b = binary.BigEndian.AppendUint32(b, w)
		b = binary.BigEndian.AppendUint32(b, h)
	}
	return b
}

// TestProbeHeader_DimensionsRequired checks TIFF and HEIC sizes are read from
// the header, and that a header without them is refused rather than decoded
// unbounded.
func TestProbeHeader_DimensionsRequired(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		w, h    int
		wantErr error
	}{
		{"tiff with size", tiffWithIFD([][2]uint32{{0x0100, 640}, {0x0101, 480}}), 640, 480, nil},
		{"tiff without size", tiffWithIFD([][2]uint32{{0x0103, 1}}), 0, 0, ErrCorrupt},
		{"heic with ispe", heicPrefix(true, 4032, 3024), 4032, 3024, nil},
		{"heic without ispe", heicPrefix(false, 0, 0), 0, 0, ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, w, h, err := probeHeader(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || w != tc.w || h != tc.h {
				t.Fatalf("got %dx%d, %v; want %dx%d", w, h, err, tc.w, tc.h)
			}
		})
	}
}
