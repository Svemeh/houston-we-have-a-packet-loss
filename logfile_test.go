package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogFileAppendLoadAndPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "ping.jsonl")
	logFile, err := OpenLogFile[PingSample](path)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	now := time.Now().Truncate(time.Second)
	for minutesAgo := 10; minutesAgo >= 0; minutesAgo-- {
		sample := PingSample{Timestamp: now.Add(-time.Duration(minutesAgo) * time.Minute), Target: "1.1.1.1", RTTMs: 40}
		if err := logFile.Append(sample); err != nil {
			t.Fatal(err)
		}
	}

	recent, err := LoadSamplesSince[PingSample](path, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 6 {
		t.Fatalf("loaded %d samples from the last 5 min, want 6", len(recent))
	}

	if err := logFile.Prune(now.Add(-2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Appends must still work after the file was swapped underneath.
	if err := logFile.Append(PingSample{Timestamp: now.Add(time.Second), Target: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}

	all, err := LoadSamplesSince[PingSample](path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("after prune + append: %d samples, want 4", len(all))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("prune left its temp file behind")
	}
}

func TestLoadSamplesSkipsCorruptLinesAndMissingFile(t *testing.T) {
	dir := t.TempDir()

	missing, err := LoadSamplesSince[PingSample](filepath.Join(dir, "nope.jsonl"), time.Time{})
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing file: got %d samples, err %v; want none, nil", len(missing), err)
	}

	path := filepath.Join(dir, "ping.jsonl")
	content := `{"timestamp":"2026-09-22T10:00:00Z","target":"1.1.1.1","rtt_ms":40,"lost":false}
{"timestamp":"2026-09-22T10:00:01Z","tar
{"timestamp":"2026-09-22T10:00:02Z","target":"1.1.1.1","lost":true}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := LoadSamplesSince[PingSample](path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[1].Lost {
		t.Errorf("got %+v, want the two intact lines", samples)
	}
}
