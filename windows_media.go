package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/iyear/gowidevine"
)

type wmSample struct {
	mp4.FullSample
	PTS     int64
	Episode *big.Rat
	IDR     bool
}
type wmVideo struct {
	Init          *mp4.InitSegment
	Track         *mp4.TrakBox
	Entry         *mp4.VisualSampleEntryBox
	Scale         int64
	Config        string
	Samples       []wmSample
	EditMediaTime int64
	EditDuration  *big.Rat
}

func inspectWindowInit(data []byte) (*wmVideo, error) {
	f, e := mp4.DecodeFile(bytes.NewReader(data))
	if e != nil || f.Init == nil || len(f.Init.Moov.Traks) != 1 {
		return nil, wf("unsupported_capability")
	}
	t := f.Init.Moov.Traks[0]
	if t.Mdia == nil || t.Mdia.Hdlr == nil || t.Mdia.Hdlr.HandlerType != "vide" || t.Mdia.Mdhd == nil || t.Mdia.Mdhd.Timescale == 0 || f.Init.Moov.Mvex == nil {
		return nil, wf("unsupported_capability")
	}
	var editMediaTime int64
	var editDuration *big.Rat
	if t.Edts != nil {
		if len(t.Edts.Elst) != 1 || len(t.Edts.Elst[0].Entries) != 1 {
			return nil, wf("timestamp_mapping_unavailable")
		}
		edit := t.Edts.Elst[0].Entries[0]
		if edit.MediaTime < 0 || edit.MediaRateInteger != 1 || edit.MediaRateFraction != 0 {
			return nil, wf("timestamp_mapping_unavailable")
		}
		editMediaTime = edit.MediaTime
		if edit.SegmentDuration != 0 {
			if f.Init.Moov.Mvhd == nil || f.Init.Moov.Mvhd.Timescale == 0 {
				return nil, wf("timestamp_mapping_unavailable")
			}
			editDuration = new(big.Rat).SetFrac(new(big.Int).SetUint64(edit.SegmentDuration), new(big.Int).SetUint64(uint64(f.Init.Moov.Mvhd.Timescale)))
		}
	}
	if t.Mdia.Minf == nil || t.Mdia.Minf.Stbl == nil || t.Mdia.Minf.Dinf == nil || t.Mdia.Minf.Dinf.Dref == nil {
		return nil, wf("unsupported_capability")
	}
	dref := t.Mdia.Minf.Dinf.Dref
	if len(dref.Children) != 1 {
		return nil, wf("unsupported_capability")
	}
	location, ok := dref.Children[0].(*mp4.URLBox)
	if !ok || location.Flags&1 == 0 || location.Location != "" {
		return nil, wf("unsupported_capability")
	}
	stsd := t.Mdia.Minf.Stbl.Stsd
	if stsd == nil || len(stsd.Children) != 1 {
		return nil, wf("unsupported_capability")
	}
	entry := stsd.AvcX
	if entry == nil {
		entry = stsd.Encv
	}
	if entry == nil || entry.AvcC == nil || len(entry.AvcC.SPSnalus) == 0 || len(entry.AvcC.PPSnalus) == 0 {
		return nil, wf("unsupported_capability")
	}
	if entry.DataReferenceIndex != 1 {
		return nil, wf("unsupported_capability")
	}
	for _, nalu := range entry.AvcC.SPSnalus {
		sps, e := avc.ParseSPSNALUnit(nalu, true)
		if e != nil || !sps.FrameMbsOnlyFlag || sps.BitDepthLumaMinus8 != 0 || sps.BitDepthChromaMinus8 != 0 || sps.ChromaFormatIDC != 1 {
			return nil, wf("unsupported_capability")
		}
		if sps.VUI != nil && sps.VUI.ColourDescriptionFlag {
			switch sps.VUI.TransferCharacteristics {
			case 16, 18:
				return nil, wf("unsupported_capability")
			}
		}
	}
	var config bytes.Buffer
	if e = entry.AvcC.Encode(&config); e != nil {
		return nil, wf("output_validation_failed")
	}
	return &wmVideo{Init: f.Init, Track: t, Entry: entry, Scale: int64(t.Mdia.Mdhd.Timescale), Config: windowHash(config.Bytes()[8:]), EditMediaTime: editMediaTime, EditDuration: editDuration}, nil
}

// IDR slices reset the H.264 reference picture state. An I picture or container
// sync flag does not. Reject unsupported extensions and changed in-band SPS/PPS.
func windowSampleIDR(data []byte, entry *mp4.VisualSampleEntryBox) (bool, error) {
	idr, vcl, nonIDR := false, false, false
	for len(data) > 0 {
		if len(data) < 4 {
			return false, wf("decode_validation_failed")
		}
		n := int(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if n == 0 || n > len(data) {
			return false, wf("decode_validation_failed")
		}
		nal := data[:n]
		data = data[n:]
		if nal[0]&0x80 != 0 {
			return false, wf("decode_validation_failed")
		}
		switch nal[0] & 31 {
		case 5:
			idr = true
			vcl = true
		case 1:
			nonIDR = true
			vcl = true
		case 7, 8:
			known := entry.AvcC.SPSnalus
			if nal[0]&31 == 8 {
				known = entry.AvcC.PPSnalus
			}
			match := false
			for _, x := range known {
				match = match || bytes.Equal(x, nal)
			}
			if !match {
				return false, wf("codec_configuration_changed")
			}
		case 6, 9, 10, 11, 12: // SEI, access-unit delimiter, end, filler
		default:
			return false, wf("dependency_information_unavailable")
		}
	}
	if !vcl || idr && nonIDR {
		return false, wf("dependency_information_unavailable")
	}
	return idr, nil
}

func loadWindowSamples(ctx context.Context, span wmSpan, indexes []int, keys []*widevine.Key, fetch wmFetch) (*wmVideo, error) {
	var video *wmVideo
	var bytesRead int64
	for _, index := range indexes {
		seg := span.Segments[index]
		data, e := fetch(ctx, seg.URL, seg.Range, 128<<20)
		if e != nil {
			return nil, e
		}
		bytesRead += int64(len(data))
		if bytesRead > 512<<20 {
			return nil, wf("unsupported_capability")
		}
		input := append(append([]byte{}, span.Init...), data...)
		init, e := inspectWindowInit(span.Init)
		if e != nil {
			return nil, e
		}
		if init.Entry.Sinf != nil {
			var clear bytes.Buffer
			if e = decryptMP4(span.Init, bytes.NewReader(input), keys, &clear); e != nil {
				return nil, wf("source_unavailable")
			}
			input = clear.Bytes()
		}
		f, e := mp4.DecodeFile(bytes.NewReader(input))
		if e != nil || f.Init == nil {
			return nil, wf("output_validation_failed")
		}
		var initBytes bytes.Buffer
		if e = f.Init.Encode(&initBytes); e != nil {
			return nil, wf("output_validation_failed")
		}
		v, e := inspectWindowInit(initBytes.Bytes())
		if e != nil {
			return nil, e
		}
		if video == nil {
			video = v
		} else if video.Config != v.Config || video.Scale != v.Scale || video.Entry.Width != v.Entry.Width || video.Entry.Height != v.Entry.Height {
			return nil, wf("codec_configuration_changed")
		}
		trex := f.Init.Moov.Mvex.Trex
		for _, segment := range f.Segments {
			for _, fragment := range segment.Fragments {
				if len(fragment.Moof.Trafs) != 1 || fragment.Moof.Traf.Tfdt == nil || fragment.Moof.Traf.Tfhd.TrackID != video.Track.Tkhd.TrackID {
					return nil, wf("timestamp_mapping_unavailable")
				}
				samples, e := fragment.GetFullSamples(trex)
				if e != nil {
					return nil, wf("output_validation_failed")
				}
				for _, s := range samples {
					if s.Dur == 0 || s.DecodeTime > math.MaxInt64-uint64(s.Dur)-uint64(math.MaxInt32) {
						return nil, wf("timestamp_mapping_unavailable")
					}
					if len(video.Samples) > 0 {
						last := video.Samples[len(video.Samples)-1]
						if s.DecodeTime != last.DecodeTime+uint64(last.Dur) {
							return nil, wf("coverage_incomplete")
						}
					}
					pts := int64(s.DecodeTime) + int64(s.CompositionTimeOffset)
					idr, e := windowSampleIDR(s.Data, video.Entry)
					if e != nil {
						return nil, e
					}
					// DASH sample time is after the edit list; PTO is then applied.
					// Keep the raw source PTS/DTS in the sidecar separately.
					// https://dashif.org/Guidelines-TimingModel/#sample-timeline
					presentation := sub(ticks(pts, video.Scale), ticks(video.EditMediaTime, video.Scale))
					episode := add(span.Start, sub(presentation, span.PTO))
					video.Samples = append(video.Samples, wmSample{s, pts, episode, idr})
				}
			}
		}
	}
	if video == nil || len(video.Samples) == 0 {
		return nil, wf("coverage_incomplete")
	}
	return video, nil
}

type wmInterval struct{ Start, End *big.Rat }

func wmIntersects(a, b wmInterval) bool { return a.Start.Cmp(b.End) < 0 && b.Start.Cmp(a.End) < 0 }
func wmCovered(want wmInterval, samples []wmSample, scale int64) bool {
	// A missing encoded sample is never a rounding discrepancy.
	for i := 1; i < len(samples); i++ {
		if samples[i].DecodeTime != samples[i-1].DecodeTime+uint64(samples[i-1].Dur) {
			return false
		}
	}
	intervals := make([]wmInterval, 0, len(samples))
	for _, s := range samples {
		intervals = append(intervals, wmInterval{s.Episode, add(s.Episode, ticks(int64(s.Dur), scale))})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].Start.Cmp(intervals[j].Start) < 0 })
	// trun durations advance the decode clock. With reordered pictures and a
	// rounded clock (e.g. 24000/1001 fps in 10 MHz), their pairing with PTS can
	// differ by one tick. Use the next actual PTS at those internal boundaries;
	// never change output timestamps, extend outer coverage, or bridge a larger
	// gap. The complete decode sequence above is still mandatory.
	for i := 0; i+1 < len(intervals); i++ {
		if intervals[i+1].Start.Cmp(intervals[i].Start) <= 0 {
			return false
		}
		delta := sub(intervals[i+1].Start, intervals[i].End)
		if new(big.Rat).Abs(delta).Cmp(ticks(1, scale)) <= 0 {
			intervals[i].End = intervals[i+1].Start
		}
	}
	cursor := want.Start
	for _, x := range intervals {
		if x.End.Cmp(cursor) <= 0 {
			continue
		}
		if x.Start.Cmp(cursor) > 0 {
			return false
		}
		cursor = x.End
		if cursor.Cmp(want.End) >= 0 {
			return true
		}
	}
	return false
}
func selectWindowSamples(v *wmVideo, span wmSpan, want wmInterval, before, after *big.Rat, reachesEnd bool) ([]wmSample, error) {
	if v.EditDuration != nil && want.End.Cmp(add(span.Start, sub(v.EditDuration, span.PTO))) > 0 {
		return nil, wf("coverage_incomplete")
	}
	start := -1
	end := len(v.Samples)
	for i, s := range v.Samples {
		if s.IDR && s.Episode.Cmp(want.Start) <= 0 {
			start = i
		}
		if s.IDR && s.Episode.Cmp(want.End) >= 0 {
			end = i
			break
		}
	}
	if start < 0 {
		return nil, wf("dependency_information_unavailable")
	}
	if end == len(v.Samples) && !reachesEnd {
		return nil, wf("dependency_information_unavailable")
	}
	selected := v.Samples[start:end]
	if len(selected) == 0 || !wmCovered(want, selected, v.Scale) {
		return nil, wf("coverage_incomplete")
	}
	lo, hi := wmSampleExtent(selected, v.Scale)
	if sub(want.Start, lo).Cmp(before) > 0 || sub(hi, want.End).Cmp(after) > 0 {
		return nil, wf("dependency_expansion_exceeds_limit")
	}
	return selected, nil
}
func wmSampleExtent(samples []wmSample, scale int64) (*big.Rat, *big.Rat) {
	lo := samples[0].Episode
	hi := lo
	for _, s := range samples {
		lo = minRat(lo, s.Episode)
		hi = maxRat(hi, add(s.Episode, ticks(int64(s.Dur), scale)))
	}
	return lo, hi
}

func writeWindowClip(ctx context.Context, v *wmVideo, samples []wmSample, destination, clipID string, requested, contexts []wmInterval) (map[string]any, error) {
	if e := writeWindowMP4(ctx, v, samples, destination); e != nil {
		return nil, e
	}
	base := samples[0].DecodeTime
	rows := []map[string]any{}
	for i, s := range samples {
		if ctx.Err() != nil {
			return nil, wf("cancelled")
		}
		interval := wmInterval{s.Episode, add(s.Episode, ticks(int64(s.Dur), v.Scale))}
		role := "decode_dependency"
		for _, x := range contexts {
			if wmIntersects(interval, x) {
				role = "analysis_context"
			}
		}
		for _, x := range requested {
			if wmIntersects(interval, x) {
				role = "requested_content"
			}
		}
		hash := windowHash(s.Data)
		row := map[string]any{"schema": "cva.media-sample-map", "version": 1, "clip_id": clipID, "sample_index": i, "clip_pts": strconv.FormatInt(s.PTS-int64(base), 10), "clip_dts": strconv.FormatUint(s.DecodeTime-base, 10), "clip_duration_ticks": strconv.FormatUint(uint64(s.Dur), 10), "source_pts": strconv.FormatInt(s.PTS, 10), "source_dts": strconv.FormatUint(s.DecodeTime, 10), "source_duration_ticks": strconv.FormatUint(uint64(s.Dur), 10), "episode_time_seconds": rs(s.Episode), "role": role, "random_access": s.IDR, "source_sample_sha256": hash, "clip_sample_sha256": hash}
		rows = append(rows, row)
	}
	properties, packets, e := validateWindowClip(ctx, destination, v, samples)
	if e != nil {
		return nil, e
	}
	timelinePath := strings.TrimSuffix(destination, ".mp4") + ".timeline.jsonl"
	side, e := os.OpenFile(timelinePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	defer side.Close()
	for i, packet := range packets {
		rows[i]["clip_pts"] = strconv.FormatInt(packet.PTS, 10)
		rows[i]["clip_dts"] = strconv.FormatInt(packet.DTS, 10)
		rows[i]["clip_duration_ticks"] = strconv.FormatInt(packet.Duration, 10)
		if _, e = side.Write(windowJSON(rows[i])); e != nil {
			return nil, e
		}
	}
	if e = side.Sync(); e != nil {
		return nil, e
	}
	if e = side.Close(); e != nil {
		return nil, e
	}
	return properties, nil
}

func findWindowProbe(ffmpeg string) (string, error) {
	p := filepath.Join(filepath.Dir(ffmpeg), "ffprobe"+filepath.Ext(ffmpeg))
	if f, e := os.Stat(p); e == nil && f.Mode().IsRegular() {
		return p, nil
	}
	p, e := exec.LookPath("ffprobe")
	if e != nil {
		return "", wf("unsupported_capability")
	}
	return p, nil
}

type wmProbePacket struct {
	PTS      int64  `json:"pts"`
	DTS      int64  `json:"dts"`
	Duration int64  `json:"duration"`
	Hash     string `json:"data_hash"`
}
type wmProbeStream struct {
	CodecType  string `json:"codec_type"`
	Codec      string `json:"codec_name"`
	Tag        string `json:"codec_tag_string"`
	Profile    string `json:"profile"`
	Level      int    `json:"level"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Format     string `json:"pix_fmt"`
	TimeBase   string `json:"time_base"`
	SAR        string `json:"sample_aspect_ratio"`
	Range      string `json:"color_range"`
	Matrix     string `json:"color_space"`
	Transfer   string `json:"color_transfer"`
	Primaries  string `json:"color_primaries"`
	ConfigHash string `json:"extradata_hash"`
	ReadFrames string `json:"nb_read_frames"`
}
type wmLimitedOutput struct {
	bytes.Buffer
	limit int
}

func (b *wmLimitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, io.ErrShortBuffer
	}
	return b.Buffer.Write(p)
}
func windowCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	out := &wmLimitedOutput{limit: 64 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if e := cmd.Run(); e != nil {
		return nil, wf("decode_validation_failed")
	}
	return out.Bytes(), nil
}
func validateWindowClip(ctx context.Context, path string, v *wmVideo, samples []wmSample) (properties map[string]any, packets []wmProbePacket, err error) {
	ffmpeg, e := findFFmpeg()
	if e != nil {
		return nil, nil, wf("unsupported_capability")
	}
	probe, e := findWindowProbe(ffmpeg)
	if e != nil {
		return nil, nil, e
	}
	b, e := windowCommand(ctx, probe, "-v", "error", "-protocol_whitelist", "file", "-count_frames", "-show_streams", "-show_packets", "-show_data_hash", "sha256", "-of", "json", path)
	if e != nil {
		return nil, nil, e
	}
	var p struct {
		Streams []wmProbeStream `json:"streams"`
		Packets []wmProbePacket `json:"packets"`
	}
	if json.Unmarshal(b, &p) != nil || len(p.Streams) != 1 || p.Streams[0].CodecType != "video" || len(p.Packets) != len(samples) {
		return nil, nil, wf("output_validation_failed")
	}
	s := p.Streams[0]
	frameCount, e := strconv.Atoi(s.ReadFrames)
	if e != nil || frameCount != len(samples) || s.TimeBase != rs(ticks(1, v.Scale)) || s.Width != int(v.Entry.Width) || s.Height != int(v.Entry.Height) || !strings.EqualFold(s.ConfigHash, "SHA256:"+v.Config) {
		return nil, nil, wf("output_validation_failed")
	}
	native, e := readWindowTiming(path, len(samples), v.Scale)
	if e != nil {
		return nil, nil, e
	}
	ptsShift := sub(big.NewRat(p.Packets[0].PTS, 1), big.NewRat(samples[0].PTS, 1))
	dtsShift := sub(big.NewRat(p.Packets[0].DTS, 1), new(big.Rat).SetInt(new(big.Int).SetUint64(samples[0].DecodeTime)))
	for i, packet := range p.Packets {
		source := samples[i]
		ps := sub(big.NewRat(packet.PTS, 1), big.NewRat(source.PTS, 1))
		ds := sub(big.NewRat(packet.DTS, 1), new(big.Rat).SetInt(new(big.Int).SetUint64(source.DecodeTime)))
		if ps.Cmp(ptsShift) != 0 || ds.Cmp(dtsShift) != 0 || native[i].Duration != int64(source.Dur) || packet.PTS != native[i].PTS || packet.DTS != native[i].DTS || !strings.EqualFold(packet.Hash, "SHA256:"+windowHash(source.Data)) {
			return nil, nil, wf("output_validation_failed")
		}
		// Use the exact final STTS value in the receipt, never the codec parser's
		// nominal duration estimate (which varies across FFprobe releases).
		p.Packets[i].Duration = native[i].Duration
	}
	// Decode the entire selected window offline. This is stronger than the
	// contract's minimum boundary check, and bounds are small donor windows.
	if _, e = windowCommand(ctx, ffmpeg, "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-protocol_whitelist", "file", "-i", path, "-map", "0:v:0", "-f", "null", "-"); e != nil {
		return nil, nil, e
	}
	nullable := func(s string) any {
		if s == "" || s == "unknown" || s == "unspecified" {
			return nil
		}
		return s
	}
	sar := strings.ReplaceAll(s.SAR, ":", "/")
	r, ok := new(big.Rat).SetString(sar)
	if !ok || r.Sign() <= 0 {
		return nil, nil, wf("output_validation_failed")
	}
	if s.Codec != "h264" || s.Format != "yuv420p" || s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67" {
		return nil, nil, wf("unsupported_capability")
	}
	return map[string]any{"id": "", "codec": s.Codec, "codec_tag": nullable(s.Tag), "profile": nullable(s.Profile), "level": s.Level, "codec_configuration_sha256": v.Config, "width": s.Width, "height": s.Height, "pixel_format": s.Format, "bit_depth": 8, "sample_aspect_ratio": rs(r), "average_frame_rate": nil, "color": map[string]any{"range": nullable(s.Range), "matrix": nullable(s.Matrix), "primaries": nullable(s.Primaries), "transfer": nullable(s.Transfer)}}, p.Packets, nil
}

func wmClipResult(path, clipID, period string, samples []wmSample, scale int64) (map[string]any, error) {
	h, n, e := windowFileHash(path)
	if e != nil {
		return nil, e
	}
	timeline := strings.TrimSuffix(path, ".mp4") + ".timeline.jsonl"
	th, _, e := windowFileHash(timeline)
	if e != nil {
		return nil, e
	}
	lo, hi := wmSampleExtent(samples, scale)
	return map[string]any{"clip_id": clipID, "path": filepath.Base(path), "bytes": n, "sha256": h, "stream_index": 0, "source_period_id": period, "discontinuity_id": nil, "source_time_base": rs(ticks(1, scale)), "clip_time_base": rs(ticks(1, scale)), "delivered_episode_seconds": []string{rs(lo), rs(hi)}, "timeline": map[string]any{"path": filepath.Base(timeline), "sha256": th, "schema": "cva.media-sample-map", "version": 1}, "assembly": map[string]any{"video_reencoded": false, "bitstream_filters": []string{}}, "validation": map[string]any{"level": "probe_and_boundary_decode", "self_contained": true, "decode_dependencies_complete": true, "requested_coverage_complete": true, "discontinuities": []string{}}}, nil
}

func wmMediaToolsAvailable() error {
	ff, e := findFFmpeg()
	if e != nil {
		return wf("unsupported_capability")
	}
	_, e = findWindowProbe(ff)
	return e
}
