package logic

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/binlog"
	"github.com/github/gh-ost/go/mysql"
	"github.com/github/gh-ost/go/sql"
	"github.com/go-mysql-org/go-mysql/replication"
)

type Coordinator struct {
	migrationContext *base.MigrationContext

	binlogSyncer *replication.BinlogSyncer

	onChangelogEvent func(dmlEvent *binlog.BinlogDMLEvent) error

	applier *Applier

	// Atomic counter for number of active workers
	busyWorkers atomic.Int64

	// Mutex protecting currentCoordinates
	currentCoordinatesMutex sync.Mutex
	// The binlog coordinates of the low water mark transaction.
	currentCoordinates mysql.BinlogCoordinates

	// Mutex to protect the fields below
	mu sync.Mutex

	// list of workers
	workers []*Worker

	// The low water mark. This is the sequence number of the last job that has been committed.
	lowWaterMark int64

	// This is a map of completed jobs by their sequence numbers.
	// This is used when updating the low water mark.
	// It records the binlog coordinates of the completed transaction.
	completedJobs map[int64]*mysql.BinlogCoordinates

	// These are the jobs that are waiting for a previous job to complete.
	// They are indexed by the sequence number of the job they are waiting for.
	waitingJobs map[int64][]chan struct{}

	events chan *replication.BinlogEvent

	workerQueue chan *Worker

	finishedMigrating atomic.Bool
}

// Worker takes jobs from the Coordinator and applies the job's DML events.
type Worker struct {
	id          int
	coordinator *Coordinator
	eventQueue  chan *replication.BinlogEvent

	executedJobs     atomic.Int64
	dmlEventsApplied atomic.Int64
	waitTimeNs       atomic.Int64
	busyTimeNs       atomic.Int64
}

type stats struct {
	dmlRate          float64
	trxRate          float64
	dmlEventsApplied int64
	executedJobs     int64
	busyTime         time.Duration
	waitTime         time.Duration
}

func (w *Worker) ProcessEvents() error {
	databaseName := w.coordinator.migrationContext.DatabaseName
	originalTableName := w.coordinator.migrationContext.OriginalTableName
	changelogTableName := w.coordinator.migrationContext.GetChangelogTableName()

	for {
		if w.coordinator.finishedMigrating.Load() {
			return nil
		}
		ev := <-w.eventQueue
		// fmt.Printf("Worker %d processing event: %T\n", w.id, ev.Event)

		// Verify this is a GTID Event
		gtidEvent, ok := ev.Event.(*replication.GTIDEvent)
		if !ok {
			w.coordinator.migrationContext.Log.Debugf("Received unexpected event: %v\n", ev)
		}

		// Wait for conditions to be met
		waitChannel := w.coordinator.WaitForTransaction(gtidEvent.LastCommitted)
		if waitChannel != nil {
			waitStart := time.Now()
			<-waitChannel
			timeWaited := time.Since(waitStart)
			w.waitTimeNs.Add(timeWaited.Nanoseconds())
		}

		// Process the transaction
		var changelogEvent *binlog.BinlogDMLEvent
		dmlEvents := make([]*binlog.BinlogDMLEvent, 0, int(atomic.LoadInt64(&w.coordinator.migrationContext.DMLBatchSize)))
	events:
		for {
			ev := <-w.eventQueue
			if ev == nil {
				fmt.Printf("Worker %d ending transaction early\n", w.id)
				break events
			}

			// fmt.Printf("Worker %d processing event: %T\n", w.id, ev.Event)

			switch binlogEvent := ev.Event.(type) {
			case *replication.RowsEvent:
				// Is this an event that we're interested in?
				// We're only interested in events that:
				// * affect the table we're migrating
				// * affect the changelog table

				dml := binlog.ToEventDML(ev.Header.EventType.String())
				if dml == binlog.NotDML {
					return fmt.Errorf("unknown DML type: %s", ev.Header.EventType.String())
				}

				if !strings.EqualFold(databaseName, string(binlogEvent.Table.Schema)) {
					continue
				}

				if !strings.EqualFold(originalTableName, string(binlogEvent.Table.Table)) && !strings.EqualFold(changelogTableName, string(binlogEvent.Table.Table)) {
					continue
				}

				for i, row := range binlogEvent.Rows {
					if dml == binlog.UpdateDML && i%2 == 1 {
						// An update has two rows (WHERE+SET)
						// We do both at the same time
						continue
					}
					dmlEvent := binlog.NewBinlogDMLEvent(
						string(binlogEvent.Table.Schema),
						string(binlogEvent.Table.Table),
						dml,
					)
					switch dml {
					case binlog.InsertDML:
						{
							dmlEvent.NewColumnValues = sql.ToColumnValues(row)
						}
					case binlog.UpdateDML:
						{
							dmlEvent.WhereColumnValues = sql.ToColumnValues(row)
							dmlEvent.NewColumnValues = sql.ToColumnValues(binlogEvent.Rows[i+1])
						}
					case binlog.DeleteDML:
						{
							dmlEvent.WhereColumnValues = sql.ToColumnValues(row)
						}
					}

					if strings.EqualFold(changelogTableName, string(binlogEvent.Table.Table)) {
						// If this is a change on the changelog table, queue it up to be processed after
						// the end of the transaction.
						changelogEvent = dmlEvent
					} else {
						dmlEvents = append(dmlEvents, dmlEvent)

						if len(dmlEvents) == cap(dmlEvents) {
							if err := w.applyDMLEvents(dmlEvents); err != nil {
								w.coordinator.migrationContext.Log.Errore(err)
							}
							dmlEvents = dmlEvents[:0]
						}
					}
				}
			case *replication.XIDEvent:
				if len(dmlEvents) > 0 {
					if err := w.applyDMLEvents(dmlEvents); err != nil {
						w.coordinator.migrationContext.Log.Errore(err)
					}
				}

				w.executedJobs.Add(1)
				break events
			}
		}

		w.coordinator.MarkTransactionCompleted(gtidEvent.SequenceNumber, int64(ev.Header.LogPos), int64(ev.Header.EventSize))

		// Did we see a changelog event?
		// Handle it now
		if changelogEvent != nil {
			// wait for all transactions before this point
			waitChannel = w.coordinator.WaitForTransaction(gtidEvent.SequenceNumber - 1)
			if waitChannel != nil {
				waitStart := time.Now()
				<-waitChannel
				w.waitTimeNs.Add(time.Since(waitStart).Nanoseconds())
			}
			w.coordinator.HandleChangeLogEvent(changelogEvent)
		}

		w.coordinator.workerQueue <- w
		w.coordinator.busyWorkers.Add(-1)
	}
}

func (w *Worker) applyDMLEvents(dmlEvents []*binlog.BinlogDMLEvent) error {
	busyStart := time.Now()
	err := w.coordinator.applier.ApplyDMLEventQueries(dmlEvents)
	if err != nil {
		//TODO(meiji163) add retry
		return err
	}
	w.busyTimeNs.Add(time.Since(busyStart).Nanoseconds())
	w.dmlEventsApplied.Add(int64(len(dmlEvents)))
	return nil
}

func NewCoordinator(migrationContext *base.MigrationContext, applier *Applier, onChangelogEvent func(dmlEvent *binlog.BinlogDMLEvent) error) *Coordinator {
	connectionConfig := migrationContext.InspectorConnectionConfig

	return &Coordinator{
		migrationContext: migrationContext,

		onChangelogEvent: onChangelogEvent,

		currentCoordinates: mysql.BinlogCoordinates{},

		binlogSyncer: replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
			ServerID:                uint32(migrationContext.ReplicaServerId),
			Flavor:                  gomysql.MySQLFlavor,
			Host:                    connectionConfig.Key.Hostname,
			Port:                    uint16(connectionConfig.Key.Port),
			User:                    connectionConfig.User,
			Password:                connectionConfig.Password,
			TLSConfig:               connectionConfig.TLSConfig(),
			UseDecimal:              true,
			MaxReconnectAttempts:    migrationContext.BinlogSyncerMaxReconnectAttempts,
			TimestampStringLocation: time.UTC,
		}),

		lowWaterMark:  0,
		completedJobs: make(map[int64]*mysql.BinlogCoordinates),
		waitingJobs:   make(map[int64][]chan struct{}),

		events: make(chan *replication.BinlogEvent, 1000),

		workerQueue: make(chan *Worker, 16),
	}
}

func (c *Coordinator) StartStreaming(canStopStreaming func() bool) error {
	ctx := context.TODO()
	streamer, err := c.binlogSyncer.StartSync(gomysql.Position{
		Name: c.currentCoordinates.LogFile,
		Pos:  uint32(c.currentCoordinates.LogPos),
	})
	if err != nil {
		return err
	}

	var retries int64
	for {
		if canStopStreaming() {
			return nil
		}
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			coords := c.GetCurrentBinlogCoordinates()
			if retries >= c.migrationContext.MaxRetries() {
				return fmt.Errorf("%d successive failures in streamer reconnect at coordinates %+v", retries, coords)
			}
			c.migrationContext.Log.Infof("Reconnecting... Will resume at %+v", coords)
			retries += 1
			// We reconnect at the position of the last low water mark.
			// Some jobs after low water mark may have already applied, but
			// it's OK to reapply them since the DML operations are idempotent.
			streamer, err = c.binlogSyncer.StartSync(gomysql.Position{
				Name: coords.LogFile,
				Pos:  uint32(coords.LogPos),
			})
			if err != nil {
				return err
			}
		}
		c.events <- ev
	}
}

func (c *Coordinator) ProcessEventsUntilNextChangelogEvent() (*binlog.BinlogDMLEvent, error) {
	databaseName := c.migrationContext.DatabaseName
	changelogTableName := c.migrationContext.GetChangelogTableName()

	for ev := range c.events {
		switch binlogEvent := ev.Event.(type) {
		case *replication.RowsEvent:
			dml := binlog.ToEventDML(ev.Header.EventType.String())
			if dml == binlog.NotDML {
				return nil, fmt.Errorf("unknown DML type: %s", ev.Header.EventType.String())
			}

			if !strings.EqualFold(databaseName, string(binlogEvent.Table.Schema)) {
				continue
			}

			if !strings.EqualFold(changelogTableName, string(binlogEvent.Table.Table)) {
				continue
			}

			for i, row := range binlogEvent.Rows {
				if dml == binlog.UpdateDML && i%2 == 1 {
					// An update has two rows (WHERE+SET)
					// We do both at the same time
					continue
				}
				dmlEvent := binlog.NewBinlogDMLEvent(
					string(binlogEvent.Table.Schema),
					string(binlogEvent.Table.Table),
					dml,
				)
				switch dml {
				case binlog.InsertDML:
					{
						dmlEvent.NewColumnValues = sql.ToColumnValues(row)
					}
				case binlog.UpdateDML:
					{
						dmlEvent.WhereColumnValues = sql.ToColumnValues(row)
						dmlEvent.NewColumnValues = sql.ToColumnValues(binlogEvent.Rows[i+1])
					}
				case binlog.DeleteDML:
					{
						dmlEvent.WhereColumnValues = sql.ToColumnValues(row)
					}
				}

				return dmlEvent, nil
			}
		}
	}

	return nil, nil
}

func (c *Coordinator) ProcessEventsUntilDrained() error {
	for {
		select {
		// Read events from the binlog and submit them to the next worker
		case ev := <-c.events:
			{
				if c.finishedMigrating.Load() {
					return nil
				}

				switch binlogEvent := ev.Event.(type) {
				case *replication.GTIDEvent:
					if c.lowWaterMark == 0 && binlogEvent.SequenceNumber > 0 {
						c.lowWaterMark = binlogEvent.SequenceNumber - 1
					}
				case *replication.RotateEvent:
					c.currentCoordinatesMutex.Lock()
					c.currentCoordinates.LogFile = string(binlogEvent.NextLogName)
					c.currentCoordinatesMutex.Unlock()
					c.migrationContext.Log.Infof("rotate to next log from %s:%d to %s", c.currentCoordinates.LogFile, int64(ev.Header.LogPos), binlogEvent.NextLogName)
					continue
				default: // ignore all other events
					continue
				}

				worker := <-c.workerQueue
				c.busyWorkers.Add(1)

				worker.eventQueue <- ev

				ev = <-c.events

				switch binlogEvent := ev.Event.(type) {
				case *replication.QueryEvent:
					if bytes.Equal([]byte("BEGIN"), binlogEvent.Query) {
						// c.migrationContext.Log.Infof("BEGIN for transaction in schema %s", binlogEvent.Schema)
					} else {
						worker.eventQueue <- nil
						continue
					}
				default:
					worker.eventQueue <- nil
					continue
				}

			events:
				for {
					ev = <-c.events
					switch ev.Event.(type) {
					case *replication.RowsEvent:
						worker.eventQueue <- ev
					case *replication.XIDEvent:
						worker.eventQueue <- ev

						// We're done with this transaction
						break events
					}
				}
			}

		// No events in the queue. Check if all workers are sleeping now
		default:
			{
				busyWorkers := c.busyWorkers.Load()
				if busyWorkers == 0 {
					return nil
				}
			}
		}
	}
}

func (c *Coordinator) InitializeWorkers(count int) {
	c.workerQueue = make(chan *Worker, count)
	for i := 0; i < count; i++ {
		w := &Worker{id: i, coordinator: c, eventQueue: make(chan *replication.BinlogEvent, 1000)}

		c.mu.Lock()
		c.workers = append(c.workers, w)
		c.mu.Unlock()

		c.workerQueue <- w
		go w.ProcessEvents()
	}
}

func (c *Coordinator) GetWorkerStats() []stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	statSlice := make([]stats, 0, len(c.workers))
	for _, w := range c.workers {
		stat := stats{}
		stat.dmlEventsApplied = w.dmlEventsApplied.Load()
		stat.executedJobs = w.executedJobs.Load()
		stat.busyTime = time.Duration(w.busyTimeNs.Load())
		stat.waitTime = time.Duration(w.waitTimeNs.Load())
		if stat.busyTime.Milliseconds() > 0 {
			stat.dmlRate = 1000.0 * float64(stat.dmlEventsApplied) / float64(stat.busyTime.Milliseconds())
			stat.trxRate = 1000.0 * float64(stat.executedJobs) / float64(stat.busyTime.Milliseconds())
		}
		statSlice = append(statSlice, stat)
	}
	return statSlice
}

func (c *Coordinator) WaitForTransaction(lastCommitted int64) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	if lastCommitted <= c.lowWaterMark {
		return nil
	}

	if _, ok := c.completedJobs[lastCommitted]; ok {
		return nil
	}

	waitChannel := make(chan struct{})
	c.waitingJobs[lastCommitted] = append(c.waitingJobs[lastCommitted], waitChannel)

	return waitChannel
}

func (c *Coordinator) HandleChangeLogEvent(event *binlog.BinlogDMLEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onChangelogEvent(event)
}

func (c *Coordinator) MarkTransactionCompleted(sequenceNumber, logPos, eventSize int64) {
	var channelsToNotify []chan struct{}
	var lastCoords *mysql.BinlogCoordinates

	func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		//c.migrationContext.Log.Infof("Coordinator: Marking job as completed: %d\n", sequenceNumber)

		// Mark the job as completed
		c.completedJobs[sequenceNumber] = &mysql.BinlogCoordinates{LogPos: logPos, EventSize: eventSize}

		// Then, update the low water mark if possible
		for {
			if coords, ok := c.completedJobs[c.lowWaterMark+1]; ok {
				lastCoords = coords
				c.lowWaterMark++
				delete(c.completedJobs, c.lowWaterMark)
			} else {
				break
			}
		}
		channelsToNotify = make([]chan struct{}, 0)

		// Schedule any jobs that were waiting for this job to complete
		for waitingForSequenceNumber, channels := range c.waitingJobs {
			if waitingForSequenceNumber <= c.lowWaterMark {
				channelsToNotify = append(channelsToNotify, channels...)
				delete(c.waitingJobs, waitingForSequenceNumber)
			}
		}

	}()

	// update the binlog coords of the low water mark
	if lastCoords != nil {
		func() {
			// c.migrationContext.Log.Infof("Updating binlog coordinates to %s:%d\n", c.currentCoordinates.LogFile, c.currentCoordinates.LogPos)
			c.currentCoordinatesMutex.Lock()
			defer c.currentCoordinatesMutex.Unlock()
			c.currentCoordinates.LogPos = lastCoords.LogPos
			c.currentCoordinates.EventSize = lastCoords.EventSize
		}()
	}

	for _, waitChannel := range channelsToNotify {
		waitChannel <- struct{}{}
	}
}

func (c *Coordinator) GetCurrentBinlogCoordinates() *mysql.BinlogCoordinates {
	c.currentCoordinatesMutex.Lock()
	defer c.currentCoordinatesMutex.Unlock()
	returnCoordinates := c.currentCoordinates
	return &returnCoordinates
}

func (c *Coordinator) Teardown() {
	c.finishedMigrating.Store(true)
}
