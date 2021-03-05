package astits

import (
	"bytes"
	"context"
	"errors"
	"github.com/asticode/go-astikit"
	"io"
)

const (
	StartPID           uint16 = 0x0100
	PMTStartPID        uint16 = 0x1000
	ProgramNumberStart uint16 = 1
)

var (
	MuxerErrorPIDNotFound      = errors.New("PID not found")
	MuxerErrorPIDAlreadyExists = errors.New("PID already exists")
	MuxerErrorPCRPIDInvalid    = errors.New("PCR PID invalid")
)

type Muxer struct {
	ctx        context.Context
	w          io.Writer
	bitsWriter *astikit.BitsWriter

	packetSize             int
	tablesRetransmitPeriod int // period in PES packets

	pm         programMap // pid -> programNumber
	pmt        PMTData
	nextPID    uint16
	patVersion wrappingCounter
	pmtVersion wrappingCounter

	patBytes bytes.Buffer
	pmtBytes bytes.Buffer

	buf       bytes.Buffer
	bufWriter *astikit.BitsWriter

	esContexts              map[uint16]*esContext
	tablesRetransmitCounter int
}

type esContext struct {
	es *PMTElementaryStream
	cc wrappingCounter
}

func newEsContext(es *PMTElementaryStream) *esContext {
	return &esContext{
		es: es,
		cc: newWrappingCounter(0b1111), // CC is 4 bits
	}
}

func MuxerOptionTablesRetransmitPeriod(newPeriod int) func(*Muxer) {
	return func(m *Muxer) {
		m.tablesRetransmitPeriod = newPeriod
	}
}

func NewMuxer(ctx context.Context, w io.Writer, opts ...func(*Muxer)) *Muxer {
	m := &Muxer{
		ctx: ctx,
		w:   w,

		packetSize:             MpegTsPacketSize, // no 192-byte packet support yet
		tablesRetransmitPeriod: 40,

		pm: newProgramMap(),
		pmt: PMTData{
			ElementaryStreams: []*PMTElementaryStream{},
			ProgramNumber:     ProgramNumberStart,
		},

		// table version is 5-bit field
		patVersion: newWrappingCounter(0b11111),
		pmtVersion: newWrappingCounter(0b11111),

		esContexts: map[uint16]*esContext{},
	}

	m.bufWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	m.bitsWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.w})

	// TODO multiple programs support
	m.pm.set(PMTStartPID, ProgramNumberStart)

	for _, opt := range opts {
		opt(m)
	}

	// to output tables at the very start
	m.tablesRetransmitCounter = m.tablesRetransmitPeriod

	return m
}

// if es.ElementaryPID is zero, it will be generated automatically
func (m *Muxer) AddElementaryStream(es PMTElementaryStream, isPCRPid bool) error {
	if es.ElementaryPID != 0 {
		for _, oes := range m.pmt.ElementaryStreams {
			if oes.ElementaryPID == es.ElementaryPID {
				return MuxerErrorPIDAlreadyExists
			}
		}
	} else {
		es.ElementaryPID = m.nextPID
		m.nextPID++
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams, &es)
	if isPCRPid {
		m.pmt.PCRPID = es.ElementaryPID
	}

	m.esContexts[es.ElementaryPID] = newEsContext(&es)
	// invalidate pmt cache
	m.pmtBytes.Reset()
	return nil
}

func (m *Muxer) RemoveElementaryStream(pid uint16) error {
	foundIdx := -1
	for i, oes := range m.pmt.ElementaryStreams {
		if oes.ElementaryPID == pid {
			foundIdx = i
			break
		}
	}

	if foundIdx == -1 {
		return MuxerErrorPIDNotFound
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams[:foundIdx], m.pmt.ElementaryStreams[foundIdx+1:]...)
	delete(m.esContexts, pid)
	m.pmtBytes.Reset()
	return nil
}

func (m *Muxer) WritePayload(pid uint16, af *PacketAdaptationField, ph *PESHeader, payload []byte) (int, error) {
	ctx, ok := m.esContexts[pid]
	if !ok {
		return 0, MuxerErrorPIDNotFound
	}

	bytesWritten := 0

	forceTables := af != nil && af.RandomAccessIndicator && pid == m.pmt.PCRPID
	n, err := m.retransmitTables(forceTables)
	if err != nil {
		return n, err
	}

	bytesWritten += n

	payloadStart := true
	writeAf := af != nil
	payloadBytesWritten := 0
	for payloadBytesWritten < len(payload) {
		pktLen := 1 + MpegTsPacketHeaderSize // sync byte + header
		pkt := Packet{
			Header: &PacketHeader{
				ContinuityCounter:         uint8(ctx.cc.get()),
				HasAdaptationField:        writeAf,
				HasPayload:                false,
				PayloadUnitStartIndicator: false,
				PID:                       pid,
			},
		}

		if writeAf {
			pkt.AdaptationField = af
			// one byte for adaptation field length field
			pktLen += 1 + int(calcPacketAdaptationFieldLength(af))
			writeAf = false
		}

		bytesAvailable := m.packetSize - pktLen
		if payloadStart {
			pesHeaderLength := PESHeaderLength + int(calcPESOptionalHeaderLength(ph.OptionalHeader))
			// af with pes header are too big, we don't have space to write pes header
			if bytesAvailable < pesHeaderLength {
				af.StuffingLength = bytesAvailable
			} else {
				//bytesAvailable -= pesHeaderLength
				pkt.Header.HasPayload = true
				pkt.Header.PayloadUnitStartIndicator = true
			}
		} else {
			pkt.Header.HasPayload = true
		}

		if pkt.Header.HasPayload {
			m.buf.Reset()
			if ph.StreamID == 0 {
				ph.StreamID = pmtStreamTypeToPESStreamID(ctx.es.StreamType)
			}

			ntot, npayload, err := writePESData(m.bufWriter, ph, payload[payloadBytesWritten:], payloadStart, bytesAvailable)
			if err != nil {
				return bytesWritten, err
			}

			payloadBytesWritten += npayload

			pkt.Payload = m.buf.Bytes()

			bytesAvailable -= ntot
			// if we still have some space in packet, we should stuff it with adaptation field stuffing
			// we can't stuff packets with 0xff at the end of a packet since it's not uncommon for PES payloads to have length unspecified
			if bytesAvailable > 0 {
				pkt.Header.HasAdaptationField = true
				if pkt.AdaptationField == nil {
					pkt.AdaptationField = newStuffingAdaptationField(bytesAvailable)
				} else {
					pkt.AdaptationField.StuffingLength = bytesAvailable
				}
			}

			n, err = writePacket(m.bitsWriter, &pkt, m.packetSize)
			if err != nil {
				return bytesWritten, err
			}

			bytesWritten += n

			payloadStart = false
		}
	}

	return bytesWritten, nil
}

func (m *Muxer) retransmitTables(force bool) (int, error) {
	m.tablesRetransmitCounter++
	if !force && m.tablesRetransmitCounter < m.tablesRetransmitPeriod {
		return 0, nil
	}

	n, err := m.WriteTables()
	if err != nil {
		return n, err
	}

	m.tablesRetransmitCounter = 0
	return n, nil
}

func (m *Muxer) WriteTables() (int, error) {
	bytesWritten := 0

	if m.patBytes.Len() != m.packetSize {
		if err := m.generatePAT(); err != nil {
			return bytesWritten, err
		}
	}

	if m.pmtBytes.Len() != m.packetSize {
		if err := m.generatePMT(); err != nil {
			return bytesWritten, err
		}
	}

	n, err := m.w.Write(m.patBytes.Bytes())
	if err != nil {
		return bytesWritten, err
	}
	bytesWritten += n

	n, err = m.w.Write(m.pmtBytes.Bytes())
	if err != nil {
		return bytesWritten, err
	}
	bytesWritten += n

	return bytesWritten, nil
}

func (m *Muxer) generatePAT() error {
	d := m.pm.toPATData()
	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PAT: d},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PAT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: d.TransportStreamID,
			VersionNumber:    uint8(m.patVersion.get()),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPATSectionLength(d),
			SectionSyntaxIndicator: true,
			TableID:                PSITableTypeId(d.TransportStreamID),
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.patBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.patBytes})

	pkt := Packet{
		Header: &PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PIDPAT,
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PAT and rollback to it here maybe?
		return err
	}

	return nil
}

func (m *Muxer) generatePMT() error {
	hasPCRPID := false
	for _, es := range m.pmt.ElementaryStreams {
		if es.ElementaryPID == m.pmt.PCRPID {
			hasPCRPID = true
			break
		}
	}
	if !hasPCRPID {
		return MuxerErrorPCRPIDInvalid
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PMT: &m.pmt},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PMT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: m.pmt.ProgramNumber,
			VersionNumber:    uint8(m.pmtVersion.get()),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPMTSectionLength(&m.pmt),
			SectionSyntaxIndicator: true,
			TableID:                PSITableTypeIdPMT,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.pmtBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.pmtBytes})

	pkt := Packet{
		Header: &PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PMTStartPID, // FIXME multiple programs support
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PMT and rollback to it here maybe?
		return err
	}

	return nil
}

// TODO move it somewhere
func pmtStreamTypeToPESStreamID(pmtStreamType StreamType) uint8 {
	switch pmtStreamType {
	case StreamTypeMPEG1Video, StreamTypeMPEG2Video, StreamTypeMPEG4Video, StreamTypeH264Video,
		StreamTypeH265Video, StreamTypeCAVSVideo, StreamTypeVC1Video:
		return 0xe0
	case StreamTypeDIRACVideo:
		return 0xfd
	case StreamTypeMPEG2Audio, StreamTypeAACAudio, StreamTypeAACLATMAudio:
		return 0xc0
	case StreamTypeAC3Audio, StreamTypeEAC3Audio: // m2ts_mode???
		return 0xfd
	case StreamTypePrivateSection, StreamTypePrivateData, StreamTypeMetadata:
		return 0xfc
	default:
		return 0xbd
	}
}
