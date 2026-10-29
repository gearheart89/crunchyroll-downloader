package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Model the downloaded AAC track's 10 MHz media clock, which differs from its
// 48 kHz sample rate. FFmpeg can report a misleading format start_time for this
// combination with an edit list, despite near-zero packet timestamps. Using
// that reported start to independently rebase each input desynchronizes A/V.
// The fixture contains only locally generated sine-wave audio.
func rescaleAudioFixture(t *testing.T, source, destination string, timescale uint32) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	file, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if file.Init == nil || len(file.Init.Moov.Traks) != 1 || len(file.Segments) == 0 {
		t.Fatal("expected a single-track fragmented audio fixture")
	}
	trak := file.Init.Moov.Traks[0]
	oldScale := trak.Mdia.Mdhd.Timescale
	scale := func(value int64) int64 {
		return int64(math.Round(float64(value) * float64(timescale) / float64(oldScale)))
	}
	trak.Mdia.Mdhd.Timescale = timescale
	trak.Mdia.Mdhd.Duration = uint64(scale(int64(trak.Mdia.Mdhd.Duration)))
	if trak.Edts == nil || len(trak.Edts.Elst) != 1 || len(trak.Edts.Elst[0].Entries) == 0 {
		t.Fatal("audio fixture must include its encoder-priming edit")
	}
	for i := range trak.Edts.Elst[0].Entries {
		entry := &trak.Edts.Elst[0].Entries[i]
		if entry.MediaTime != -1 {
			entry.MediaTime = scale(entry.MediaTime)
		}
		// SegmentDuration uses the movie clock, which has not changed.
	}
	for _, trex := range file.Init.Moov.Mvex.Trexs {
		trex.DefaultSampleDuration = uint32(scale(int64(trex.DefaultSampleDuration)))
	}
	for _, segment := range file.Segments {
		for _, fragment := range segment.Fragments {
			for _, traf := range fragment.Moof.Trafs {
				traf.Tfdt.SetBaseMediaDecodeTime(uint64(scale(int64(traf.Tfdt.BaseMediaDecodeTime()))))
				traf.Tfhd.DefaultSampleDuration = uint32(scale(int64(traf.Tfhd.DefaultSampleDuration)))
				for _, trun := range traf.Truns {
					for i := range trun.Samples {
						sample := &trun.Samples[i]
						sample.Dur = uint32(scale(int64(sample.Dur)))
						sample.CompositionTimeOffset = int32(scale(int64(sample.CompositionTimeOffset)))
					}
				}
			}
		}
	}
	var encoded bytes.Buffer
	if err := file.Encode(&encoded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

type timedPacket struct {
	PTS  string `json:"pts_time"`
	Hash string `json:"data_hash"`
}

func probeTimedPackets(t *testing.T, ffprobe, file, stream string) []timedPacket {
	t.Helper()
	var result struct {
		Packets []timedPacket
	}
	data := runMediaTool(t, ffprobe, "-v", "error", "-select_streams", stream,
		"-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=pts_time,data_hash", "-of", "json", file)
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Packets) == 0 {
		t.Fatal("no packets in fixture")
	}
	return result.Packets
}

func packetTime(t *testing.T, packet timedPacket) float64 {
	t.Helper()
	value, err := strconv.ParseFloat(packet.PTS, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestMuxPreservesPacketTiming(t *testing.T) {
	ffmpeg, ffprobe := requireMediaTools(t)
	dir := t.TempDir()
	standardAudio := filepath.Join(dir, "48k.m4a")
	runMediaTool(t, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-c:a", "aac", "-movflags", "+delay_moov+frag_keyframe+default_base_moof", "-frag_duration", "500000", standardAudio)
	scaledAudio := filepath.Join(dir, "10MHz.m4a")
	rescaleAudioFixture(t, standardAudio, scaledAudio, 10000000)
	audio := []mediaTrack{{file: scaledAudio, locale: "en-US"}, {file: standardAudio, locale: "ja-JP"}}
	subtitle := filepath.Join(dir, "source.srt")
	subtitleBytes := []byte("1\n00:00:00,100 --> 00:00:01,800\nTiming fixture\n")
	if err := os.WriteFile(subtitle, subtitleBytes, 0600); err != nil {
		t.Fatal(err)
	}
	subs := []mediaTrack{{file: subtitle, locale: "en-US", format: "srt"}}
	for _, videoOffset := range []string{"0", "0.25"} {
		video := filepath.Join(dir, "video-"+videoOffset+".mp4")
		runMediaTool(t, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=64x64:r=24:d=2",
			"-c:v", "mpeg4", "-bf", "2", "-an", "-output_ts_offset", videoOffset, video)
		for _, format := range []string{"mkv", "mp4"} {
			t.Run(format+"/video_offset_"+videoOffset, func(t *testing.T) {
				output := filepath.Join(dir, "output-"+videoOffset+"."+format)
				if err := writeOutput(video, audio, subs, output, EpisodeInfo{}); err != nil {
					t.Fatal(err)
				}
				inputs := []struct{ path, sourceStream, outputStream string }{
					{video, "v:0", "v:0"}, {scaledAudio, "a:0", "a:0"}, {standardAudio, "a:0", "a:1"},
				}
				if format == "mkv" {
					inputs = append(inputs, struct{ path, sourceStream, outputStream string }{subtitle, "s:0", "s:0"})
				} else {
					assertOutputBytes(t, filepath.Join(dir, "output-"+videoOffset+".en.srt"), subtitleBytes)
				}
				var commonShift float64
				for index, input := range inputs {
					before := probeTimedPackets(t, ffprobe, input.path, input.sourceStream)
					after := probeTimedPackets(t, ffprobe, output, input.outputStream)
					if len(before) != len(after) {
						t.Fatalf("%s lost or added packets: %d -> %d", input.outputStream, len(before), len(after))
					}
					if index == 0 {
						commonShift = packetTime(t, after[0]) - packetTime(t, before[0])
					}
					for i := range before {
						if before[i].Hash != after[i].Hash {
							t.Fatalf("%s packet %d payload changed", input.outputStream, i)
						}
						shift := packetTime(t, after[i]) - packetTime(t, before[i])
						// Matroska rounds to milliseconds. Allow that rounding,
						// but require the same shift for every packet and track.
						if math.Abs(shift-commonShift) > 0.002 {
							t.Fatalf("%s packet %d moved by %.6fs; video moved by %.6fs (A/V/subtitle sync changed)", input.outputStream, i, shift, commonShift)
						}
					}
				}
				if math.Abs(commonShift) > 0.2 {
					t.Fatalf("unexpected multi-second output delay: %.6fs", commonShift)
				}
			})
		}
	}
}
