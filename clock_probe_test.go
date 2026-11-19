package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Eyevinn/mp4ff/mp4"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic 10-MHz source clock; no provider bytes, credentials or network.
func TestCVAClockProbe(t *testing.T) {
	f := windowFixture(t, false)
	span := &f.spans[0]
	init, e := inspectWindowInit(span.Init)
	if e != nil {
		t.Fatal(e)
	}
	oldScale := init.Scale
	const scale int64 = 10000000
	rounded := func(tick int64) int64 { return (tick*scale + oldScale/2) / oldScale }
	init.Track.Mdia.Mdhd.Timescale = uint32(scale)
	init.Track.Mdia.Mdhd.Duration = uint64(rounded(int64(init.Track.Mdia.Mdhd.Duration)))
	var b bytes.Buffer
	if e = init.Init.Encode(&b); e != nil {
		t.Fatal(e)
	}
	span.Init = append([]byte{}, b.Bytes()...)
	for u, data := range f.assets {
		file, e := mp4.DecodeFile(bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		b.Reset()
		for _, segment := range file.Segments {
			for i, frag := range segment.Fragments {
				samples, e := frag.GetFullSamples(init.Init.Moov.Mvex.Trex)
				if e != nil {
					t.Fatal(e)
				}
				out, e := mp4.CreateFragment(uint32(i+1), init.Track.Tkhd.TrackID)
				if e != nil {
					t.Fatal(e)
				}
				for _, s := range samples {
					d := int64(s.DecodeTime)
					p := d + int64(s.CompositionTimeOffset)
					s.Dur = uint32(rounded(d+int64(s.Dur)) - rounded(d))
					s.DecodeTime = uint64(rounded(d))
					s.CompositionTimeOffset = int32(rounded(p) - rounded(d))
					out.AddFullSample(s)
				}
				if e = out.Encode(&b); e != nil {
					t.Fatal(e)
				}
			}
		}
		f.assets[u] = append([]byte{}, b.Bytes()...)
	}
	indexes := make([]int, len(span.Segments))
	for i := range indexes {
		indexes[i] = i
	}
	v, e := loadWindowSamples(context.Background(), *span, indexes, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "clock.mp4")
	_, e = writeWindowClip(context.Background(), v, v.Samples, path, "clock", []wmInterval{{rat("0/1"), rat("12/1")}}, nil)
	if e == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		container, err := mp4.DecodeFile(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		stts := container.Moov.Traks[0].Mdia.Minf.Stbl.Stts
		for i, s := range v.Samples {
			if got := stts.GetDur(uint32(i + 1)); got != s.Dur {
				t.Fatalf("actual STTS sample %d: source=%d output=%d", i, s.Dur, got)
			}
		}
		if got := stts.GetDur(uint32(len(v.Samples))); got != 416667 {
			t.Fatalf("terminal sample duration changed: %d", got)
		}
		t.Log("10-MHz source passes strict qualification; all 288 actual STTS durations, including final 416667 ticks, preserved")
		return
	}
	ff, _ := findFFmpeg()
	probe, _ := findWindowProbe(ff)
	data, pe := windowCommand(context.Background(), probe, "-v", "error", "-show_streams", "-show_packets", "-show_data_hash", "sha256", "-of", "json", path)
	if pe != nil {
		t.Fatalf("write error %v; probe error %v", e, pe)
	}
	var observed struct {
		Streams []wmProbeStream `json:"streams"`
		Packets []wmProbePacket `json:"packets"`
	}
	if pe = json.Unmarshal(data, &observed); pe != nil {
		t.Fatal(pe)
	}
	t.Logf("write=%v; source_samples=%d packets=%d source_scale=%d probe_timebase=%s", e, len(v.Samples), len(observed.Packets), scale, observed.Streams[0].TimeBase)
	raw, _ := os.ReadFile(path)
	container, _ := mp4.DecodeFile(bytes.NewReader(raw))
	for _, flags := range []string{"+nofillin", "+noparse+nofillin"} {
		data, fe := windowCommand(context.Background(), probe, "-v", "error", "-fflags", flags, "-show_packets", "-of", "json", path)
		var alternative struct {
			Packets []wmProbePacket `json:"packets"`
		}
		json.Unmarshal(data, &alternative)
		if fe == nil && len(alternative.Packets) > 0 {
			t.Logf("fflags=%s first duration=%d", flags, alternative.Packets[0].Duration)
		}
	}
	for i, p := range observed.Packets {
		s := v.Samples[i]
		if p.Duration != int64(s.Dur) {
			t.Fatalf("duration mismatch at packet %d: source=%d ffprobe=%d actual_STTS=%d ticks (one tick = 100 ns); payload_matches=%v", i, s.Dur, p.Duration, container.Moov.Traks[0].Mdia.Minf.Stbl.Stts.GetDur(uint32(i+1)), p.Hash == "SHA256:"+windowHash(s.Data))
		}
	}
	t.Fatalf("write failed without a duration mismatch: %v", e)
}
