package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func windowTableFixture(t *testing.T) (*wmVideo, []byte) {
	t.Helper()
	f := windowFixture(t, false)
	v, e := loadWindowSamples(context.Background(), f.spans[0], []int{0, 1, 2}, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "tables.mp4")
	if e = writeWindowMP4(context.Background(), v, v.Samples, path); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return v, b
}

func TestWindowTimingTablesRejectMalformedInput(t *testing.T) {
	v, original := windowTableFixture(t)
	for _, mode := range []string{"valid", "truncated-file", "truncated-header", "zero-top-size", "oversized-moov", "missing-stts", "duplicate-stts", "stts-version", "stts-flags", "stts-zero-run", "stts-zero-duration", "stts-short-count", "stts-excess-count", "stts-entry-count", "stsz-count", "mdhd-scale", "mdhd-duration", "ctts-short-count", "ctts-excess-count", "ctts-version", "edit-list", "fragmented", "extra-track", "extra-mdat"} {
		t.Run(mode, func(t *testing.T) {
			data := append([]byte{}, original...)
			file, e := mp4.DecodeFile(bytes.NewReader(data))
			if e != nil {
				t.Fatal(e)
			}
			track := file.Moov.Traks[0]
			stbl := track.Mdia.Minf.Stbl
			switch mode {
			case "missing-stts":
				for i, c := range stbl.Children {
					if c.Type() == "stts" {
						stbl.Children = append(stbl.Children[:i], stbl.Children[i+1:]...)
						break
					}
				}
			case "duplicate-stts":
				stbl.AddChild(stbl.Stts)
			case "stts-version":
				stbl.Stts.Version = 1
			case "stts-flags":
				stbl.Stts.Flags = 1
			case "stts-zero-run":
				stbl.Stts.SampleCount[0] = 0
			case "stts-zero-duration":
				stbl.Stts.SampleTimeDelta[0] = 0
			case "stts-short-count":
				stbl.Stts.SampleCount[0]--
			case "stts-excess-count":
				stbl.Stts.SampleCount[0] = math.MaxUint32
			case "stsz-count":
				stbl.Stsz.SampleNumber++
			case "mdhd-scale":
				track.Mdia.Mdhd.Timescale++
			case "mdhd-duration":
				track.Mdia.Mdhd.Duration--
			case "ctts-short-count":
				stbl.Ctts.EndSampleNr[len(stbl.Ctts.EndSampleNr)-1]--
			case "ctts-excess-count":
				stbl.Ctts.EndSampleNr[len(stbl.Ctts.EndSampleNr)-1] = math.MaxUint32
			case "ctts-version":
				stbl.Ctts.Version = 2
			case "edit-list":
				track.AddChild(&mp4.EdtsBox{})
			case "fragmented":
				file.Moov.AddChild(&mp4.MvexBox{})
			case "extra-track":
				file.Moov.AddChild(track)
			}
			var out bytes.Buffer
			if e = file.Encode(&out); e != nil {
				t.Fatal(e)
			}
			data = out.Bytes()
			switch mode {
			case "truncated-file":
				data = data[:len(data)-1]
			case "truncated-header":
				data = append(data, []byte{0, 0, 0}...)
			case "zero-top-size":
				binary.BigEndian.PutUint32(data[:4], 0)
			case "oversized-moov":
				binary.BigEndian.PutUint32(data[file.Ftyp.Size():file.Ftyp.Size()+4], math.MaxUint32)
			case "stts-entry-count":
				pos := bytes.Index(data, []byte("stts"))
				binary.BigEndian.PutUint32(data[pos+8:pos+12], math.MaxUint32)
			case "extra-mdat":
				data = append(data, 0, 0, 0, 8, 'm', 'd', 'a', 't')
			}
			path := filepath.Join(t.TempDir(), "candidate.mp4")
			if e = os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
			packets, e := readWindowTiming(path, len(v.Samples), v.Scale)
			if mode == "valid" {
				if e != nil || len(packets) != len(v.Samples) {
					t.Fatal("valid tables rejected", e)
				}
			} else if windowErrorCode(e) != "output_validation_failed" {
				t.Fatal("malformed timing accepted", e)
			}
		})
	}
}

func TestWindowChangedTerminalDurationIsStillRejected(t *testing.T) {
	v, data := windowTableFixture(t)
	file, e := mp4.DecodeFile(bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	track := file.Moov.Traks[0]
	stbl := track.Mdia.Minf.Stbl
	stts := stbl.Stts
	last := len(stts.SampleCount) - 1
	if stts.SampleCount[last] > 1 {
		stts.SampleCount[last]--
		stts.SampleCount = append(stts.SampleCount, 1)
		stts.SampleTimeDelta = append(stts.SampleTimeDelta, stts.SampleTimeDelta[last])
	}
	stts.SampleTimeDelta[len(stts.SampleTimeDelta)-1]--
	track.Mdia.Mdhd.Duration--
	stbl.Co64.ChunkOffset[0] = file.Ftyp.Size() + file.Moov.Size() + 8
	var out bytes.Buffer
	if e = file.Encode(&out); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "changed-terminal.mp4")
	if e = os.WriteFile(path, out.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	// The altered container is internally consistent; only the comparison with
	// the actual source durations detects its last-sample loss.
	packets, e := readWindowTiming(path, len(v.Samples), v.Scale)
	if e != nil {
		t.Fatal(e)
	}
	if packets[len(packets)-1].Duration != int64(v.Samples[len(v.Samples)-1].Dur)-1 {
		t.Fatal("fixture did not change terminal duration")
	}
	if _, _, e = validateWindowClip(context.Background(), path, v, v.Samples); windowErrorCode(e) != "output_validation_failed" {
		t.Fatal("terminal sample change accepted", e)
	}
}

func TestWindowMP4Uses64BitDurationHeaders(t *testing.T) {
	v, _ := windowTableFixture(t)
	samples := append([]wmSample{}, v.Samples[:2]...)
	samples[0].DecodeTime = 0
	samples[0].Dur = math.MaxUint32
	samples[0].CompositionTimeOffset = 0
	samples[0].PTS = 0
	samples[1].DecodeTime = math.MaxUint32
	samples[1].Dur = 7
	samples[1].CompositionTimeOffset = 0
	samples[1].PTS = math.MaxUint32
	path := filepath.Join(t.TempDir(), "long-clock.mp4")
	if e := writeWindowMP4(context.Background(), v, samples, path); e != nil {
		t.Fatal(e)
	}
	packets, e := readWindowTiming(path, 2, v.Scale)
	if e != nil {
		t.Fatal(e)
	}
	if packets[0].Duration != math.MaxUint32 || packets[1].DTS != math.MaxUint32 || packets[1].Duration != 7 {
		t.Fatal("duration truncated")
	}
}
