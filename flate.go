package piudf

import (
	"compress/flate"
	"compress/zlib"
	"io"
)

// FlateWriter compresses stream payloads as FlateDecode, which is a zlib
// stream (ISO 32000-1 7.4.4). It owns its compressor, allocated on the first
// payload and reset for every one after, so a warm FlateWriter compresses
// without allocating.
//
// A compressed payload's length is known only once it is written, and output
// is forward-only, so its /Length is an indirect object written afterwards:
//
//	lenID := enc.NewID()
//	enc.BeginObject(id)
//	// dictionary with /Filter /FlateDecode /Length lenID 0 R
//	enc.BeginStreamPayload()
//	w, err := fw.Reset(enc.PayloadWriter())
//	// write the payload to w
//	err = fw.Close()
//	n := enc.EndStreamPayload()
//	enc.EndObject()
//	enc.WriteLength(lenID, n)
type FlateWriter struct {
	// Level is a compress/flate level. Zero means flate.DefaultCompression,
	// so flate.NoCompression cannot be asked for: an uncompressed payload is
	// one written without a FlateWriter.
	Level int

	zw    *zlib.Writer
	level int // The level zw was made with.
}

// Reset starts a payload compressed into w, usually [Encoder.PayloadWriter],
// and returns the writer its uncompressed bytes go to. It fails only on a bad
// Level.
func (f *FlateWriter) Reset(w io.Writer) (io.Writer, error) {
	level := f.Level
	if level == 0 {
		level = flate.DefaultCompression
	}
	if f.zw != nil && f.level == level {
		f.zw.Reset(w)
		return f.zw, nil
	}
	zw, err := zlib.NewWriterLevel(w, level)
	if err != nil {
		return nil, err
	}
	f.zw, f.level = zw, level
	return zw, nil
}

// Close ends the payload started by Reset, writing the rest of the compressed
// stream.
func (f *FlateWriter) Close() error {
	if f.zw == nil {
		return errEncoderState
	}
	return f.zw.Close()
}

// WriteLength writes id as a bare integer object: the indirect /Length of a
// stream whose payload, n bytes, was measured by [Encoder.EndStreamPayload].
func (e *Encoder) WriteLength(id ObjectID, n int64) {
	e.BeginObject(id)
	e.Int(n)
	e.EndObject()
}
