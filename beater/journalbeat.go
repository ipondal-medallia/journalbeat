// Copyright 2017 Marcus Heese
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package beater

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/coreos/go-systemd/sdjournal"
	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/processors"
	"github.com/elastic/beats/libbeat/publisher"
	"github.com/f0ster/go-metrics-influxdb"
	"github.com/mheese/journalbeat/config"
	"github.com/mheese/journalbeat/journal"
	"github.com/rcrowley/go-metrics"
	"github.com/wavefronthq/go-metrics-wavefront"
)

// Journalbeat is the main Journalbeat struct
type Journalbeat struct {
	done   chan struct{}
	config config.Config
	client publisher.Client

	journal *sdjournal.Journal

	cursorChan chan string

	logstashClients                 []publisher.Client
	numLogstashAvailable            int //corresponds to the number of downstream logstash aggregators available at startup.
	journalTypeOutstandingLogBuffer map[string]*LogBuffer
	incomingLogMessages             chan common.MapStr

	logMessagesPublished metrics.Counter
	logMessageDelay      metrics.Gauge
}

func (jb *Journalbeat) initJournal() error {
	var err error

	seekToHelper := func(position string, err error) error {
		if err == nil {
			logp.Info("Seek to %s successful", position)
		} else {
			logp.Warn("Could not seek to %s: %v", position, err)
		}
		return err
	}

	// connect to the Systemd Journal
	if jb.journal, err = sdjournal.NewJournal(); err != nil {
		return err
	}

	// add specific units to monitor if any
	for _, unit := range jb.config.Units {
		if err = jb.journal.AddMatch(sdjournal.SD_JOURNAL_FIELD_SYSTEMD_UNIT + "=" + unit); err != nil {
			return fmt.Errorf("Filtering unit %s failed: %v", unit, err)
		}
	}

	// seek position
	position := jb.config.SeekPosition
	// try seekToCursor first, if that is requested
	if position == config.SeekPositionCursor {
		if cursor, err := ioutil.ReadFile(jb.config.CursorStateFile); err != nil {
			logp.Warn("Could not seek to cursor: reading cursor state file failed: %v", err)
		} else {
			// try to seek to cursor and if successful return
			if err = seekToHelper(config.SeekPositionCursor, jb.journal.SeekCursor(string(cursor))); err == nil {
				return nil
			}
		}

		if jb.config.CursorSeekFallback == config.SeekPositionDefault {
			return err
		}

		position = jb.config.CursorSeekFallback
	}

	switch position {
	case config.SeekPositionHead:
		err = seekToHelper(config.SeekPositionHead, jb.journal.SeekHead())
	case config.SeekPositionTail:
		err = seekToHelper(config.SeekPositionTail, jb.journal.SeekTail())
	}

	if err != nil {
		return fmt.Errorf("Seeking to a good position in journal failed: %v", err)
	}

	return nil
}

// WriteCursorLoop runs the loop which flushes the current cursor position to a file
func (jb *Journalbeat) writeCursorLoop() {
	var cursor string
	saveCursorState := func(cursor string) {
		if cursor != "" {
			if err := ioutil.WriteFile(jb.config.CursorStateFile, []byte(cursor), 0644); err != nil {
				logp.Err("Could not write to cursor state file: %v", err)
			}
		}
	}

	// save cursor for the last time when stop signal caught
	// Saving the cursor through defer guarantees that the jb.cursorChan has been fully consumed
	// and we are writing the cursor of the last message published.
	defer func() { saveCursorState(cursor) }()

	tick := time.Tick(jb.config.CursorFlushPeriod)

	for cursor = range jb.cursorChan {
		select {
		case <-tick:
			saveCursorState(cursor)
		default:
		}
	}
}

// New creates beater
func New(b *beat.Beat, cfg *common.Config) (beat.Beater, error) {
	config := config.DefaultConfig
	var err error
	if err = cfg.Unpack(&config); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	jb := &Journalbeat{
		done:                            make(chan struct{}),
		config:                          config,
		cursorChan:                      make(chan string),
		incomingLogMessages:             make(chan common.MapStr, channelSize),
		journalTypeOutstandingLogBuffer: make(map[string]*LogBuffer),
	}

	if err = jb.initJournal(); err != nil {
		logp.Err("Failed to connect to the Systemd Journal: %v", err)
		return nil, err
	}

	return jb, nil
}

// Run is the main event loop: read from journald and pass it to Publish
func (jb *Journalbeat) Run(b *beat.Beat) error {
	logp.Info("Journalbeat is running!")

	if jb.config.MetricsEnabled {
		if jb.config.WavefrontCollector != "" {
			logp.Info("Wavefront metrics are enabled. Sending to " + jb.config.WavefrontCollector)
			addr, err := net.ResolveTCPAddr("tcp", jb.config.WavefrontCollector)
			if jb.config.WavefrontCollector != "" && err == nil {
				logp.Info("Metrics address parsed")

				// make sure the configuration is sane.
				registry := metrics.DefaultRegistry
				jb.logMessageDelay = metrics.NewRegisteredGauge("MessageConsumptionDelay", registry)
				jb.logMessagesPublished = metrics.NewRegisteredCounter("MessagesPublished", registry)

				hostname, err := os.Hostname()
				if err == nil {
					jb.config.MetricTags["source"] = hostname
				}

				wfConfig := wavefront.WavefrontConfig{
					Addr:          addr,
					Registry:      registry,
					FlushInterval: jb.config.MetricsInterval,
					DurationUnit:  time.Nanosecond,
					Prefix:        metricPrefix,
					HostTags:      jb.config.MetricTags,
					Percentiles:   []float64{0.5, 0.75, 0.95, 0.99, 0.999},
				}

				// validate if we can emit metrics to wavefront.
				if err = wavefront.WavefrontOnce(wfConfig); err != nil {
					logp.Err("Metrics collection for log processing on this host failed at boot time: %v", err)
				}

				go wavefront.WavefrontWithConfig(wfConfig)
			} else {
				logp.Err("Cannot parse the IP address of wavefront address " + jb.config.WavefrontCollector)
			}
		}

		if jb.config.InfluxDBURL != "" {
			logp.Info("InfluxDB metrics are enabled. Sending to " + jb.config.InfluxDBURL)
			go influxdb.InfluxDB(
				metrics.DefaultRegistry,   // metrics registry
				jb.config.MetricsInterval, // interval
				jb.config.InfluxDBURL,     // the InfluxDB url
				jb.config.InfluxDatabase,  // your InfluxDB database
				"",              // your InfluxDB user
				"",              // your InfluxDB password
			)
		}
	}

	defer func() {
		close(jb.cursorChan)
		jb.client.Close()
		jb.journal.Close()
	}()

	if jb.config.WriteCursorState {
		go jb.writeCursorLoop()
	}

	go jb.logProcessor()

	jb.client = b.Publisher.Connect()

	var err error
	jb.numLogstashAvailable, err = b.Config.Output["logstash"].CountField("hosts")
	if err != nil {
		logp.Err("Invalid configuration for sending contents to logstash")
		os.Exit(101)
	}

	for i := 0; i < jb.numLogstashAvailable; i++ {
		newProcessors, err := processors.New(b.Config.Processors)
		if err != nil {
			return fmt.Errorf("error initializing processors: %v", err)
		}

		// override the hosts to pick one of the entries from the original hosts configuration.
		newOutput, err := common.NewConfigFrom(b.Config.Output["logstash"])
		if err != nil {
			return fmt.Errorf("Failed to clone output config: %v", err)
		}
		err = shiftlist(b.Config.Output["logstash"], newOutput, "hosts", i)
		if err != nil {
			return fmt.Errorf("Failed to shift list %v", err)
		}

		newPublisher, err := publisher.New(b.Name, b.Version, map[string]*common.Config{"logstash": newOutput}, b.Config.Shipper, newProcessors)
		if err != nil {
			return fmt.Errorf("error initializing publisher: %v", err)
		}

		jb.logstashClients = append(jb.logstashClients, newPublisher.Connect())
	}

	commonFields := []string{hostNameField, messageField, priorityField}

	for rawEvent := range journal.Follow(jb.journal, jb.done) {
		event := common.MapStr{}
		if _, ok := rawEvent.Fields[containerIdField]; ok {
			selectedFields := append(commonFields, []string{containerTagField, containerIdField}...)
			event = MapStrFromJournalEntry(
				rawEvent,
				jb.config.CleanFieldNames,
				jb.config.ConvertToNumbers,
				jb.config.MoveMetadataLocation,
				selectedFields)
			event["type"] = "container"
			event["logBufferingType"] = rawEvent.Fields[containerIdField]
		} else {
			selectedFields := append(commonFields, []string{tagField, processField}...)
			event = MapStrFromJournalEntry(
				rawEvent,
				jb.config.CleanFieldNames,
				jb.config.ConvertToNumbers,
				jb.config.MoveMetadataLocation,
				selectedFields)
			event["type"] = rawEvent.Fields[tagField]
			event["logBufferingType"] = rawEvent.Fields[processField]
		}

		event["input_type"] = jb.config.DefaultType
		event["cursor"] = rawEvent.Cursor
		if tmStr, ok := rawEvent.Fields[timestampField]; ok {
			tm, err := strconv.ParseInt(tmStr, 10, 64)
			if err == nil {
				event["utcTimestamp"] = tm
			} else {
				event["utcTimestamp"] = int64(rawEvent.RealtimeTimestamp)
			}
		} else {
			event["utcTimestamp"] = int64(rawEvent.RealtimeTimestamp)
		}

		jb.incomingLogMessages <- event
	}
	return nil
}

// Stop stops Journalbeat execution
func (jb *Journalbeat) Stop() {
	logp.Info("Stopping Journalbeat")
	close(jb.done)
	for i := 0; i < jb.numLogstashAvailable; i++ {
		jb.logstashClients[i].Close()
	}
	logp.Info("Journalbeat stopped")
}
