package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/SAP/go-hdb/driver/internal/protocol/encoding"
)

const (
	writeLobRequestSize = 21
)

// LobOptions represents a lob option set.
type LobOptions int8

const (
	loNullindicator LobOptions = 0x01
	loDataincluded  LobOptions = 0x02
	loLastdata      LobOptions = 0x04
)

const (
	loNullindicatorText = "null indicator"
	loDataincludedText  = "data included"
	loLastdataText      = "last data"
)

func (o LobOptions) String() string {
	var s []string
	if o&loNullindicator != 0 {
		s = append(s, loNullindicatorText)
	}
	if o&loDataincluded != 0 {
		s = append(s, loDataincludedText)
	}
	if o&loLastdata != 0 {
		s = append(s, loLastdataText)
	}
	return fmt.Sprintf("%v", s)
}

// IsLastData return true if the last data package was read, false otherwise.
func (o LobOptions) IsLastData() bool { return (o & loLastdata) != 0 }
func (o LobOptions) isNull() bool     { return (o & loNullindicator) != 0 }

// lob typecode.
type lobTypecode int8

const (
	ltcUndefined lobTypecode = 0
	ltcBlob      lobTypecode = 1
	ltcClob      lobTypecode = 2
	ltcNclob     lobTypecode = 3
)

// not used
// type lobFlags bool

// func (f lobFlags) String() string { return fmt.Sprintf("%t", f) }
// func (f *lobFlags) decode(dec *encoding.Decoder, ph *partHeader) error {
// 	*f = lobFlags(dec.Bool())
// 	return dec.Error()
// }
// func (f lobFlags) encode(enc *encoding.Encoder) error { enc.Bool(bool(f)); return nil }

// LobScanner is the interface wrapping the Scan method for Lob reading.
type LobScanner interface {
	Scan(w io.Writer) error
}

// LobDecoderSetter is the interface wrapping the setDecoder method for Lob reading.
type LobDecoderSetter interface {
	SetDecoder(fn func(descr *LobOutDescr, wr io.Writer) error)
}

var _ LobScanner = (*LobOutDescr)(nil)
var _ LobDecoderSetter = (*LobOutDescr)(nil)

// LobInDescr represents a lob input descriptor.
type LobInDescr struct {
	rd  io.Reader
	Opt LobOptions
	pos int
	buf bytes.Buffer
}

func newLobInDescr(rd io.Reader) *LobInDescr {
	return &LobInDescr{rd: rd}
}

func (d *LobInDescr) String() string {
	// restrict output size
	return fmt.Sprintf("options %s size %d pos %d bytes %v", d.Opt, d.buf.Len(), d.pos, d.buf.Bytes()[:min(d.buf.Len(), 25)])
}

// FetchNext fetches the next lob chunk.
func (d *LobInDescr) FetchNext(chunkSize int) error {
	/*
		We need to guarantee, that a max amount of data is read to prevent
		piece wise LOB writing when avoidable
		--> copy up to chunkSize
	*/
	d.buf.Reset()
	_, err := io.CopyN(&d.buf, d.rd, int64(chunkSize))
	d.Opt = loDataincluded
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	d.Opt |= loLastdata
	return nil
}

func (d *LobInDescr) setPos(pos int) { d.pos = pos }

func (d *LobInDescr) size() int { return d.buf.Len() }

func (d *LobInDescr) writeFirst(enc *encoding.Encoder) { enc.Bytes(d.buf.Bytes()) }

// LocatorID represents a locotor id.
type LocatorID uint64 // byte[locatorIdSize]

// LobOutDescr represents a lob output descriptor.
type LobOutDescr struct {
	decoder     func(descr *LobOutDescr, wr io.Writer) error
	IsCharBased bool
	/*
		HDB does not return lob type code but undefined only
		--> ltc is always ltcUndefined
		--> use isCharBased instead of type code check
	*/
	ltc     lobTypecode
	Opt     LobOptions
	NumChar int64
	numByte int64
	ID      LocatorID
	B       []byte
}

func (d *LobOutDescr) String() string {
	return fmt.Sprintf("typecode %s options %s numChar %d numByte %d id %d bytes %v", d.ltc, d.Opt, d.NumChar, d.numByte, d.ID, d.B)
}

// SetDecoder implements the LobDecoderSetter interface.
func (d *LobOutDescr) SetDecoder(decoder func(descr *LobOutDescr, wr io.Writer) error) {
	d.decoder = decoder
}

// Scan implements the LobScanner interface.
func (d *LobOutDescr) Scan(wr io.Writer) error { return d.decoder(d, wr) }

/*
write lobs:
- write lob field to database in chunks
- loop:
  - writeLobRequest
  - writeLobReply
*/

// WriteLobDescr represents a lob descriptor for writes (lob -> db).
type WriteLobDescr struct {
	LobInDescr *LobInDescr
	ID         LocatorID
	Opt        LobOptions
	ofs        int64
	b          []byte
}

func (d WriteLobDescr) String() string {
	return fmt.Sprintf("id %d options %s offset %d bytes %v", d.ID, d.Opt, d.ofs, d.b)
}

// FetchNext fetches the next lob chunk.
func (d *WriteLobDescr) FetchNext(chunkSize int) error {
	if err := d.LobInDescr.FetchNext(chunkSize); err != nil {
		return err
	}
	d.Opt = d.LobInDescr.Opt
	d.ofs = -1 // offset (-1 := append)
	d.b = d.LobInDescr.buf.Bytes()
	return nil
}

// sniffer.
func (d *WriteLobDescr) decode(dec *encoding.Decoder) error {
	d.ID = LocatorID(dec.Uint64())
	d.Opt = LobOptions(dec.Int8())
	d.ofs = dec.Int64()
	size := dec.Int32()
	d.b = make([]byte, size)
	dec.Bytes(d.b)
	return nil
}

// write chunk to db.
func (d *WriteLobDescr) encode(enc *encoding.Encoder) error {
	enc.Uint64(uint64(d.ID))
	enc.Int8(int8(d.Opt))
	enc.Int64(d.ofs)
	enc.Int32(int32(len(d.b)))
	enc.Bytes(d.b)
	return nil
}

// WriteLobRequest represents a lob write request part.
type WriteLobRequest struct {
	Descrs []*WriteLobDescr
}

func (r *WriteLobRequest) String() string { return fmt.Sprintf("descriptors %v", r.Descrs) }

func (r *WriteLobRequest) size() int {
	size := 0
	for _, descr := range r.Descrs {
		size += (writeLobRequestSize + len(descr.b))
	}
	return size
}

func (r *WriteLobRequest) numArg() int { return len(r.Descrs) }

// sniffer.
func (r *WriteLobRequest) decodeNumArg(dec *encoding.Decoder, numArg int) error {
	r.Descrs = make([]*WriteLobDescr, numArg)
	for i := 0; i < numArg; i++ {
		r.Descrs[i] = &WriteLobDescr{}
		if err := r.Descrs[i].decode(dec); err != nil {
			return err
		}
	}
	return nil
}

func (r *WriteLobRequest) encode(enc *encoding.Encoder) error {
	for _, descr := range r.Descrs {
		if err := descr.encode(enc); err != nil {
			return err
		}
	}
	return nil
}

// WriteLobReply represents a lob write reply part.
type WriteLobReply struct {
	// write lob fields to db (reply)
	// - returns ids which have not been written completely
	IDs []LocatorID
}

func (r *WriteLobReply) String() string { return fmt.Sprintf("ids %v", r.IDs) }

func (r *WriteLobReply) decodeNumArg(dec *encoding.Decoder, numArg int) error {
	r.IDs = resizeSlice(r.IDs, numArg)

	for i := 0; i < numArg; i++ {
		r.IDs[i] = LocatorID(dec.Uint64())
	}
	return dec.Error()
}

// ReadLobRequest represents a lob read request part.
type ReadLobRequest struct {
	/*
	   read lobs:
	   - read lob field from database in chunks
	   - loop:
	     - readLobRequest
	     - readLobReply

	   - read lob reply
	     seems like readLobreply returns only a result for one lob - even if more then one is requested
	     --> read single lobs
	*/
	ID        LocatorID
	Ofs       int64
	ChunkSize int32
}

func (r *ReadLobRequest) String() string {
	return fmt.Sprintf("id %d offset %d size %d", r.ID, r.Ofs, r.ChunkSize)
}

// sniffer.
func (r *ReadLobRequest) decode(dec *encoding.Decoder) error {
	r.ID = LocatorID(dec.Uint64())
	r.Ofs = dec.Int64()
	r.ChunkSize = dec.Int32()
	dec.Skip(4)
	return nil
}

func (r *ReadLobRequest) encode(enc *encoding.Encoder) error {
	enc.Uint64(uint64(r.ID))
	enc.Int64(r.Ofs + 1) // 1-based
	enc.Int32(r.ChunkSize)
	enc.Zeroes(4)
	return nil
}

// ReadLobReply represents a lob read reply part.
type ReadLobReply struct {
	ID  LocatorID
	Opt LobOptions
	B   []byte
}

func (r *ReadLobReply) String() string {
	return fmt.Sprintf("id %d options %s bytes %v", r.ID, r.Opt, r.B)
}

func (r *ReadLobReply) decodeNumArg(dec *encoding.Decoder, numArg int) error {
	if numArg != 1 {
		panic("numArg == 1 expected")
	}
	r.ID = LocatorID(dec.Uint64())
	r.Opt = LobOptions(dec.Int8())
	size := int(dec.Int32())
	dec.Skip(3)
	r.B = slices.Grow(r.B, size)[:size]
	dec.Bytes(r.B)
	return nil
}
