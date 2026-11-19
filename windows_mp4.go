package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Write the final flat MP4 directly. A fragmented-to-flat FFmpeg remux can
// replace the last sample duration with a nominal codec-rate estimate. STTS and
// CTTS here are built from the original samples, including the terminal sample.
func writeWindowMP4(ctx context.Context, v *wmVideo, samples []wmSample, path string) error {
	if len(samples) == 0 || len(samples) > 1<<20 || v.Scale <= 0 || v.Scale > math.MaxUint32 {
		return wf("output_validation_failed")
	}
	var encoded bytes.Buffer
	if e := v.Init.Encode(&encoded); e != nil {
		return wf("output_validation_failed")
	}
	clone, e := mp4.DecodeFile(bytes.NewReader(encoded.Bytes()))
	if e != nil || clone.Init == nil || len(clone.Init.Moov.Traks) != 1 {
		return wf("output_validation_failed")
	}
	moov := clone.Init.Moov
	track := moov.Traks[0]
	if track.Tkhd == nil || moov.Mvhd == nil {
		return wf("output_validation_failed")
	}
	// Retain codec/color/aspect-ratio and data-reference metadata, but no source
	// fragment indexes, encryption metadata, edits, or obsolete sample tables.
	moov.Children = []mp4.Box{moov.Mvhd, track}
	moov.Mvex, moov.Pssh, moov.Psshs = nil, nil, nil
	track.Children = []mp4.Box{track.Tkhd, track.Mdia}
	track.Edts = nil
	track.Mdia.Children = []mp4.Box{track.Mdia.Mdhd, track.Mdia.Hdlr, track.Mdia.Minf}
	minf := track.Mdia.Minf
	if minf.Vmhd == nil || minf.Dinf == nil || minf.Stbl == nil || minf.Stbl.Stsd == nil {
		return wf("output_validation_failed")
	}
	stbl := &mp4.StblBox{}
	stbl.AddChild(minf.Stbl.Stsd)
	minf.Stbl = stbl
	minf.Children = []mp4.Box{minf.Vmhd, minf.Dinf, stbl}
	stts := &mp4.SttsBox{}
	ctts := &mp4.CttsBox{EndSampleNr: []uint32{0}}
	stsz := &mp4.StszBox{SampleNumber: uint32(len(samples))}
	stss := &mp4.StssBox{}
	co64 := &mp4.Co64Box{ChunkOffset: []uint64{0}}
	stsc := &mp4.StscBox{Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(samples)), FirstSampleNr: 1}}, SampleDescriptionID: []uint32{1}}
	base := samples[0].DecodeTime
	var ctsShift int64
	for _, s := range samples {
		if -int64(s.CompositionTimeOffset) > ctsShift {
			ctsShift = -int64(s.CompositionTimeOffset)
		}
	}
	// A single exact composition shift makes CTTS nonnegative, avoiding different
	// demuxer interpretations of signed CTS. Both clock shifts are measured and
	// recorded in the sidecar; the source PTS/DTS and compressed bytes stay intact.
	var duration, presentationEnd, dataSize uint64
	for i, s := range samples {
		if ctx.Err() != nil {
			return wf("cancelled")
		}
		if s.Dur == 0 || len(s.Data) == 0 || uint64(len(s.Data)) > math.MaxUint32 || s.DecodeTime > math.MaxInt64-math.MaxInt32 || s.DecodeTime < base || s.DecodeTime-base != duration || duration > math.MaxInt64-uint64(s.Dur)-math.MaxUint32 {
			return wf("timestamp_mapping_unavailable")
		}
		if s.PTS != int64(s.DecodeTime)+int64(s.CompositionTimeOffset) {
			return wf("timestamp_mapping_unavailable")
		}
		offset := int64(s.CompositionTimeOffset) + ctsShift
		if offset < 0 || offset > math.MaxUint32 {
			return wf("timestamp_mapping_unavailable")
		}
		if n := len(stts.SampleCount); n > 0 && stts.SampleTimeDelta[n-1] == s.Dur {
			stts.SampleCount[n-1]++
		} else {
			stts.SampleCount = append(stts.SampleCount, 1)
			stts.SampleTimeDelta = append(stts.SampleTimeDelta, s.Dur)
		}
		if n := len(ctts.SampleOffset); n > 0 && uint32(ctts.SampleOffset[n-1]) == uint32(offset) {
			ctts.EndSampleNr[n] = uint32(i + 1)
		} else {
			ctts.SampleOffset = append(ctts.SampleOffset, int32(offset))
			ctts.EndSampleNr = append(ctts.EndSampleNr, uint32(i+1))
		}
		stsz.SampleSize = append(stsz.SampleSize, uint32(len(s.Data)))
		if s.IDR {
			stss.SampleNumber = append(stss.SampleNumber, uint32(i+1))
		}
		duration += uint64(s.Dur)
		if end := duration + uint64(offset); end > presentationEnd {
			presentationEnd = end
		}
		dataSize += uint64(len(s.Data))
		if dataSize > 512<<20 {
			return wf("unsupported_capability")
		}
	}
	stbl.AddChild(stts)
	stbl.AddChild(ctts)
	stbl.AddChild(stsc)
	stbl.AddChild(stsz)
	stbl.AddChild(stss)
	stbl.AddChild(co64)
	track.Mdia.Mdhd.Version = 1
	track.Mdia.Mdhd.Timescale = uint32(v.Scale)
	track.Mdia.Mdhd.Duration = duration
	moov.Mvhd.Version = 1
	moov.Mvhd.Timescale = uint32(v.Scale)
	moov.Mvhd.Duration = presentationEnd
	track.Tkhd.Version = 1
	track.Tkhd.Duration = presentationEnd
	ftyp := mp4.NewFtyp("isom", 512, []string{"isom", "iso6", "avc1", "mp41"})
	if moov.Size() > 16<<20 {
		return wf("unsupported_capability")
	}
	co64.ChunkOffset[0] = ftyp.Size() + moov.Size() + 8
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return wf("output_configuration_failed")
	}
	defer f.Close()
	if e = ftyp.Encode(f); e != nil {
		return wf("output_configuration_failed")
	}
	if e = moov.Encode(f); e != nil {
		return wf("output_configuration_failed")
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(dataSize+8))
	copy(header[4:], "mdat")
	if _, e = f.Write(header[:]); e != nil {
		return wf("output_configuration_failed")
	}
	for _, s := range samples {
		if ctx.Err() != nil {
			return wf("cancelled")
		}
		if _, e = f.Write(s.Data); e != nil {
			return wf("output_configuration_failed")
		}
	}
	if e = f.Sync(); e != nil {
		return wf("output_configuration_failed")
	}
	if e = f.Close(); e != nil {
		return wf("output_configuration_failed")
	}
	return nil
}

type wmBox struct {
	kind    string
	payload []byte
}

// Bounded, independent table reader. Do not use FFprobe's packet duration as
// container authority: some builds report a nominal H.264 frame-rate estimate.
func windowBoxChildren(data []byte) ([]wmBox, error) {
	var boxes []wmBox
	for len(data) > 0 {
		if len(data) < 8 || len(boxes) >= 256 {
			return nil, wf("output_validation_failed")
		}
		size, header := uint64(binary.BigEndian.Uint32(data[:4])), uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return nil, wf("output_validation_failed")
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, wf("output_validation_failed")
		}
		boxes = append(boxes, wmBox{string(data[4:8]), data[header:size]})
		data = data[size:]
	}
	return boxes, nil
}

func windowOneBox(data []byte, kind string) ([]byte, error) {
	boxes, e := windowBoxChildren(data)
	if e != nil {
		return nil, e
	}
	var found []byte
	count := 0
	for _, b := range boxes {
		if b.kind == kind {
			found = b.payload
			count++
		}
	}
	if count != 1 {
		return nil, wf("output_validation_failed")
	}
	return found, nil
}

func readWindowTiming(path string, count int, scale int64) ([]wmProbePacket, error) {
	if count <= 0 || count > 1<<20 || scale <= 0 {
		return nil, wf("output_validation_failed")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, wf("output_validation_failed")
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil || stat.Size() > (512+17)<<20 {
		return nil, wf("output_validation_failed")
	}
	var moov []byte
	mdats := 0
	for offset, boxes := int64(0), 0; offset < stat.Size(); boxes++ {
		var h [16]byte
		if boxes >= 256 || stat.Size()-offset < 8 {
			return nil, wf("output_validation_failed")
		}
		if _, e = f.ReadAt(h[:8], offset); e != nil {
			return nil, wf("output_validation_failed")
		}
		size, header := uint64(binary.BigEndian.Uint32(h[:4])), uint64(8)
		if size == 1 {
			if _, e = f.ReadAt(h[8:], offset+8); e != nil {
				return nil, wf("output_validation_failed")
			}
			size = binary.BigEndian.Uint64(h[8:])
			header = 16
		}
		if size < header || size > uint64(stat.Size()-offset) {
			return nil, wf("output_validation_failed")
		}
		switch string(h[4:8]) {
		case "moof":
			return nil, wf("output_validation_failed")
		case "mdat":
			mdats++
		case "moov":
			if moov != nil || size > 16<<20 {
				return nil, wf("output_validation_failed")
			}
			moov = make([]byte, int(size-header))
			if _, e = f.ReadAt(moov, offset+int64(header)); e != nil {
				return nil, wf("output_validation_failed")
			}
		}
		offset += int64(size)
	}
	if moov == nil || mdats != 1 {
		return nil, wf("output_validation_failed")
	}
	children, e := windowBoxChildren(moov)
	if e != nil {
		return nil, e
	}
	for _, b := range children {
		if b.kind == "mvex" {
			return nil, wf("output_validation_failed")
		}
	}
	trak, e := windowOneBox(moov, "trak")
	if e != nil {
		return nil, e
	}
	children, e = windowBoxChildren(trak)
	if e != nil {
		return nil, e
	}
	for _, b := range children {
		if b.kind == "edts" {
			return nil, wf("output_validation_failed")
		}
	}
	mdia, e := windowOneBox(trak, "mdia")
	if e != nil {
		return nil, e
	}
	mdhd, e := windowOneBox(mdia, "mdhd")
	if e != nil {
		return nil, e
	}
	var nativeScale uint32
	var duration uint64
	if len(mdhd) == 36 && mdhd[0] == 1 {
		nativeScale = binary.BigEndian.Uint32(mdhd[20:24])
		duration = binary.BigEndian.Uint64(mdhd[24:32])
	} else if len(mdhd) == 24 && mdhd[0] == 0 {
		nativeScale = binary.BigEndian.Uint32(mdhd[12:16])
		duration = uint64(binary.BigEndian.Uint32(mdhd[16:20]))
	} else {
		return nil, wf("output_validation_failed")
	}
	if int64(nativeScale) != scale {
		return nil, wf("output_validation_failed")
	}
	hdlr, e := windowOneBox(mdia, "hdlr")
	if e != nil || len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
		return nil, wf("output_validation_failed")
	}
	minf, e := windowOneBox(mdia, "minf")
	if e != nil {
		return nil, e
	}
	stbl, e := windowOneBox(minf, "stbl")
	if e != nil {
		return nil, e
	}
	stsz, e := windowOneBox(stbl, "stsz")
	if e != nil || len(stsz) < 12 || binary.BigEndian.Uint32(stsz[:4]) != 0 || binary.BigEndian.Uint32(stsz[8:12]) != uint32(count) {
		return nil, wf("output_validation_failed")
	}
	if uniform := binary.BigEndian.Uint32(stsz[4:8]); (uniform == 0 && len(stsz) != 12+4*count) || (uniform != 0 && len(stsz) != 12) {
		return nil, wf("output_validation_failed")
	}
	stts, e := windowOneBox(stbl, "stts")
	if e != nil || len(stts) < 8 || binary.BigEndian.Uint32(stts[:4]) != 0 {
		return nil, wf("output_validation_failed")
	}
	entries := binary.BigEndian.Uint32(stts[4:8])
	if uint64(entries) > uint64(count) || uint64(len(stts)) != 8+8*uint64(entries) {
		return nil, wf("output_validation_failed")
	}
	packets := make([]wmProbePacket, 0, count)
	var dts int64
	for pos := 8; pos < len(stts); pos += 8 {
		n, delta := binary.BigEndian.Uint32(stts[pos:pos+4]), int64(binary.BigEndian.Uint32(stts[pos+4:pos+8]))
		if n == 0 || delta == 0 || uint64(n) > uint64(count-len(packets)) || uint64(n) > uint64((math.MaxInt64-dts)/delta) {
			return nil, wf("output_validation_failed")
		}
		for j := uint32(0); j < n; j++ {
			packets = append(packets, wmProbePacket{PTS: dts, DTS: dts, Duration: delta})
			dts += delta
		}
	}
	if len(packets) != count || uint64(dts) != duration {
		return nil, wf("output_validation_failed")
	}
	// The writer always emits CTTS, including zero-offset rows, and no edits.
	ctts, e := windowOneBox(stbl, "ctts")
	if e != nil || len(ctts) < 8 || ctts[0] != 0 || ctts[1] != 0 || ctts[2] != 0 || ctts[3] != 0 {
		return nil, wf("output_validation_failed")
	}
	entries = binary.BigEndian.Uint32(ctts[4:8])
	if uint64(entries) > uint64(count) || uint64(len(ctts)) != 8+8*uint64(entries) {
		return nil, wf("output_validation_failed")
	}
	index := 0
	for pos := 8; pos < len(ctts); pos += 8 {
		n, delta := binary.BigEndian.Uint32(ctts[pos:pos+4]), int64(binary.BigEndian.Uint32(ctts[pos+4:pos+8]))
		if n == 0 || uint64(n) > uint64(count-index) {
			return nil, wf("output_validation_failed")
		}
		for j := uint32(0); j < n; j++ {
			if packets[index].DTS > math.MaxInt64-delta {
				return nil, wf("output_validation_failed")
			}
			packets[index].PTS += delta
			index++
		}
	}
	if index != count {
		return nil, wf("output_validation_failed")
	}
	return packets, nil
}
