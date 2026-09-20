package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// HopResult is one echo to one hop.
type HopResult struct {
	RTTMs float64 `json:"rtt_ms,omitempty"` // absent when lost
	Lost  bool    `json:"lost"`
	Error string  `json:"error,omitempty"`
}

// HopSample is one round of echoes to every hop, sent at the same moment.
//
// The JSON tags are the on-disk log format (hops.jsonl).
type HopSample struct {
	Timestamp time.Time            `json:"timestamp"`
	Hops      map[string]HopResult `json:"hops"`
}

func (sample HopSample) SampleTime() time.Time { return sample.Timestamp }

// HopPinger pings every target in parallel once per PingInterval and logs the
// round as a single line, so every hop in a line shares the same second.
type HopPinger struct {
	pingers []*Pinger
	logFile *LogFile[HopSample]
}

func NewHopPinger(targets []string, logFile *LogFile[HopSample]) *HopPinger {
	pingers := make([]*Pinger, 0, len(targets))
	for _, target := range targets {
		pingers = append(pingers, NewPinger(target, nil, nil))
	}
	return &HopPinger{pingers: pingers, logFile: logFile}
}

// Run pings until ctx is cancelled. Like Pinger.Run, errors are logged per hop
// when they start and when they clear.
func (hopPinger *HopPinger) Run(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	lastErrors := make(map[string]string, len(hopPinger.pingers))

	pingRound := func() {
		sample := hopPinger.PingAll(ctx)
		if ctx.Err() != nil {
			return
		}
		for target, result := range sample.Hops {
			if result.Error == lastErrors[target] {
				continue
			}
			if result.Error != "" {
				log.Printf("hop %s: %s", target, result.Error)
			} else {
				log.Printf("hop %s: sending again", target)
			}
			lastErrors[target] = result.Error
		}
		if err := hopPinger.logFile.Append(sample); err != nil {
			log.Printf("hops log write failed: %v", err)
		}
	}

	pingRound()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingRound()
		}
	}
}

// PingAll sends one echo to every hop at once and waits for all of them.
func (hopPinger *HopPinger) PingAll(ctx context.Context) HopSample {
	sample := HopSample{Timestamp: time.Now(), Hops: make(map[string]HopResult, len(hopPinger.pingers))}
	results := make([]PingSample, len(hopPinger.pingers))

	var waitGroup sync.WaitGroup
	for index, pinger := range hopPinger.pingers {
		waitGroup.Add(1)
		go func(index int, pinger *Pinger) {
			defer waitGroup.Done()
			results[index] = pinger.PingOnce(ctx)
		}(index, pinger)
	}
	waitGroup.Wait()

	for _, result := range results {
		sample.Hops[result.Target] = HopResult{RTTMs: result.RTTMs, Lost: result.Lost, Error: result.Error}
	}
	return sample
}
