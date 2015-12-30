//
// Copyright 2015 Gregory Trubetskoy. All Rights Reserved.
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

package timeriver

import (
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type trTransceiver struct {
	serviceMgr   *trServiceManager
	dss          *trDataSources
	dpCh         chan *trDataPoint     // incoming data point
	workerChs    []chan *trDataPoint   // incoming data point with ds
	flusherChs   []chan *trDataSource  // ds to flush
	dsCopyChs    []chan *dsCopyRequest // request a copy of a DS (used by HTTP)
	stCh         chan *trStat          // incoming statd stats
	workerWg     sync.WaitGroup
	flusherWg    sync.WaitGroup
	statWg       sync.WaitGroup
	dispatcherWg sync.WaitGroup
	startWg      sync.WaitGroup
}

type dsCopyRequest struct {
	dsId int64
	resp chan *trDataSource
}

func newTransceiver() *trTransceiver {
	dss := &trDataSources{}
	return &trTransceiver{dss: dss,
		dpCh: make(chan *trDataPoint, 1048576), // so we can survive a graceful restart
	}
}

func (t *trTransceiver) start(gracefulProtos string) error {
	t.startWorkers()
	t.startFlushers()
	t.startStatWorker()
	t.serviceMgr = newServiceManager(t)

	if err := t.serviceMgr.run(gracefulProtos); err != nil {
		return err
	}

	// Wait for workers/flushers to start correctly
	t.startWg.Wait()
	log.Printf("All workers running, good to go!")

	if gracefulProtos != "" {
		parent := syscall.Getppid()
		log.Printf("start(): Killing parent pid: %v", parent)
		syscall.Kill(parent, syscall.SIGTERM)
		log.Printf("start(): Waiting for the parent to signal that flush is complete...")
		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGUSR1)
		s := <-ch
		log.Printf("start(): Received %v, proceeding to load the data", s)
	}

	t.dss.reload() // *finally* load the data (because graceful restart)

	go t.dispatcher() // now start dispatcher

	return nil
}

func (t *trTransceiver) stop() {

	t.serviceMgr.closeListeners()
	log.Printf("Waiting for all TCP connections to finish...")
	tcpWg.Wait()
	log.Printf("TCP connections finished.")

	log.Printf("Closing dispatcher channel...")
	close(t.dpCh)
	t.dispatcherWg.Wait()
	log.Printf("Dispatcher finished.")

}

func (t *trTransceiver) stopWorkers() {
	log.Printf("stopWorkers(): waiting for worker channels to empty...")
	empty := false
	for !empty {
		empty = true
		for _, c := range t.workerChs {
			if len(c) > 0 {
				empty = false
				break
			}
		}
		if !empty {
			time.Sleep(100 * time.Millisecond)
		}
	}

	log.Printf("stopWorkers(): closing all worker channels...")
	for _, ch := range t.workerChs {
		close(ch)
	}
	log.Printf("stopWorkers(): waiting for workers to finish...")
	t.workerWg.Wait()
	log.Printf("stopWorkers(): all workers finished.")
}

func (t *trTransceiver) stopFlushers() {
	log.Printf("stopFlushers(): closing all flusher channels...")
	for _, ch := range t.flusherChs {
		close(ch)
	}
	log.Printf("stopFlushers(): waiting for flushers to finish...")
	t.flusherWg.Wait()
	log.Printf("stopFlushers(): all flushers finished.")
}

func (t *trTransceiver) stopStatWorker() {

	log.Printf("stopStatWorker(): waiting for stat channel to empty...")
	for len(t.stCh) > 0 {
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("stopStatWorker(): closing stat channel...")
	close(t.stCh)
	log.Printf("stopStatWorker(): waiting for stat worker to finish...")
	t.statWg.Wait()
	log.Printf("stopStatWorker(): stat worker finished.")
}

func (t *trTransceiver) dispatcher() {
	t.dispatcherWg.Add(1)
	defer t.dispatcherWg.Done()

	for {
		dp, ok := <-t.dpCh

		if !ok {
			log.Printf("dispatcher(): channel closed, shutting down")
			t.stopStatWorker()
			t.stopWorkers()
			t.stopFlushers()
			break
		}

		if dp.ds = t.dss.getByName(dp.Name); dp.ds == nil {
			// DS does not exist, can we create it?
			if dsSpec := config.findMatchingDsSpec(dp.Name); dsSpec != nil {
				if ds, err := createDataSource(dp.Name, dsSpec); err == nil {
					t.dss.insert(ds)
					dp.ds = ds
				} else {
					log.Printf("dispatcher(): createDataSource() error: %v", err)
					continue
				}
			}
		}

		if dp.ds != nil {
			t.workerChs[dp.ds.Id%int64(config.Workers)] <- dp
		}
	}
}

// TODO: what is the point of this one-line method?
func (t *trTransceiver) queueDataPoint(dp *trDataPoint) {
	t.dpCh <- dp
}

// TODO: what is the point of this one-line method?
func (t *trTransceiver) queueStat(st *trStat) {
	t.stCh <- st
}

func (t *trTransceiver) requestDsCopy(id int64) *trDataSource {
	req := &dsCopyRequest{id, make(chan *trDataSource)}
	t.dsCopyChs[id%int64(config.Workers)] <- req
	return <-req.resp
}

func (t *trTransceiver) worker(id int64) {

	t.workerWg.Add(1)
	defer t.workerWg.Done()

	var (
		ds              *trDataSource
		flushEverything bool
		recent          = make(map[int64]bool)
	)

	var periodicFlushCheck = make(chan int)
	go func() {
		for {
			// Sleep randomly between min and max cache durations (is this wise?)
			i := int(config.MaxCache.Duration.Nanoseconds()-config.MinCache.Duration.Nanoseconds()) / 1000
			time.Sleep(time.Duration(rand.Intn(i))*time.Millisecond + config.MinCache.Duration)
			periodicFlushCheck <- 1
		}
	}()

	log.Printf("  - worker(%d) started.", id)
	t.startWg.Done()

	for {
		ds, flushEverything = nil, false

		select {
		case <-periodicFlushCheck:
			// Nothing to do here
		case dp, ok := <-t.workerChs[id]:
			if ok {
				ds = dp.ds // at this point dp.ds has to be already set
				if err := dp.process(); err == nil {
					recent[ds.Id] = true
				} else {
					log.Printf("worker(%d): dp.process() error: %v", id, err)
				}
			} else {
				flushEverything = true
			}
		case r := <-t.dsCopyChs[id]:
			ds = t.dss.getById(r.dsId)
			if ds == nil {
				log.Printf("worker(%d): WAT? cannot lookup ds id (%d) sent for copy?", id, r.dsId)
			} else {
				r.resp <- ds.mostlyCopy()
				close(r.resp)
			}
			continue
		}

		if ds == nil {
			// flushEverything or periodic
			for dsId, _ := range recent {
				ds = t.dss.getById(dsId)
				if ds == nil {
					log.Printf("worker(%d): WAT? cannot lookup ds id (%d) to flush?", id, dsId)
					continue
				} else if flushEverything || ds.shouldBeFlushed() {
					t.flushDs(ds)
					delete(recent, ds.Id)
				}
			}
		} else if ds.shouldBeFlushed() {
			// flush just this one ds
			t.flushDs(ds)
			delete(recent, ds.Id)
		}

		if flushEverything {
			break
		}
	}
}

func (t *trTransceiver) flushDs(ds *trDataSource) {
	t.flusherChs[ds.Id%int64(config.Workers)] <- ds.mostlyCopy()
	ds.LastFlushRT = time.Now()
	ds.clearRRAs()
}

func (t *trTransceiver) startWorkers() {

	t.workerChs = make([]chan *trDataPoint, config.Workers)
	t.dsCopyChs = make([]chan *dsCopyRequest, config.Workers)

	log.Printf("Starting %d workers...", config.Workers)
	t.startWg.Add(config.Workers)
	for i := 0; i < config.Workers; i++ {
		t.workerChs[i] = make(chan *trDataPoint, 1024)
		t.dsCopyChs[i] = make(chan *dsCopyRequest, 1024)

		go t.worker(int64(i))
	}

}

func (t *trTransceiver) flusher(id int64) {
	t.flusherWg.Add(1)
	defer t.flusherWg.Done()

	log.Printf("  - flusher(%d) started.", id)
	t.startWg.Done()

	for {
		ds, ok := <-t.flusherChs[id]
		if ok {
			if err := flushDataSource(ds); err != nil {
				log.Printf("flusher(%d): error flushing data source %v: %v", id, ds, err)
			}
		} else {
			log.Printf("flusher(%d): channel closed, exiting", id)
			break
		}
	}

}

func (t *trTransceiver) startFlushers() {

	t.flusherChs = make([]chan *trDataSource, config.Workers)

	log.Printf("Starting %d flushers...", config.Workers)
	t.startWg.Add(config.Workers)
	for i := 0; i < config.Workers; i++ {
		t.flusherChs[i] = make(chan *trDataSource)
		go t.flusher(int64(i))
	}
}

func (t *trTransceiver) startStatWorker() {
	t.stCh = make(chan *trStat, 1024)
	log.Printf("Starting statWorker...")
	t.startWg.Add(1)
	go t.statWorker()
}

func (t *trTransceiver) statWorker() {

	t.statWg.Add(1)
	defer t.statWg.Done()

	var flushCh = make(chan int, 1)
	go func() {
		for {
			// NB: We do not use a time.Ticker here because my simple
			// experiments show that it will not stay aligned on a
			// multiple of durationif the system clock is
			// adjusted. This thing will mostly remain aligned.
			clock := time.Now()
			time.Sleep(clock.Truncate(config.StatFlush.Duration).Add(config.StatFlush.Duration).Sub(clock))
			if len(flushCh) == 0 {
				flushCh <- 1
			} else {
				log.Printf("statWorker(): dropping stat flush timer on the floor - busy system?")
			}
		}
	}()

	log.Printf("  - statWorker() started.")
	t.startWg.Done()

	counts := make(map[string]int64)
	gauges := make(map[string]float64)
	timers := make(map[string][]float64)

	prefix := config.StatsNamePrefix

	var flushStats = func() {
		for name, count := range counts {
			perSec := float64(count) / config.StatFlush.Duration.Seconds()
			t.queueDataPoint(&trDataPoint{Name: prefix + "." + name, TimeStamp: time.Now(), Value: perSec})
		}
		for name, gauge := range gauges {
			t.queueDataPoint(&trDataPoint{Name: prefix + ".gauges." + name, TimeStamp: time.Now(), Value: gauge})
		}
		for name, times := range timers {
			// count
			t.queueDataPoint(&trDataPoint{Name: prefix + ".timers." + name + ".count", TimeStamp: time.Now(), Value: float64(len(times))})

			// lower, upper, sum, mean
			if len(times) > 0 {
				var (
					lower, upper = times[0], times[0]
					sum          float64
				)

				for _, v := range times[1:] {
					lower = math.Min(lower, v)
					upper = math.Max(upper, v)
					sum += v
				}
				t.queueDataPoint(&trDataPoint{Name: prefix + ".timers." + name + ".lower", TimeStamp: time.Now(), Value: lower})
				t.queueDataPoint(&trDataPoint{Name: prefix + ".timers." + name + ".upper", TimeStamp: time.Now(), Value: upper})
				t.queueDataPoint(&trDataPoint{Name: prefix + ".timers." + name + ".sum", TimeStamp: time.Now(), Value: sum})
				t.queueDataPoint(&trDataPoint{Name: prefix + ".timers." + name + ".mean", TimeStamp: time.Now(), Value: sum / float64(len(times))})
			}

			// TODO - these will require sorting:
			// count_ps ?
			// mean_90
			// median
			// std
			// sum_90
			// upper_90

		}
		// clear the maps
		counts = make(map[string]int64)
		gauges = make(map[string]float64)
		timers = make(map[string][]float64)
	}

	for {
		// It's important to flush stats at as precise time as
		// possible. This non-blocking select will guarantee that we
		// process flushCh even if there is stuff in the stCh.
		select {
		case <-flushCh:
			flushStats()
		default:
		}

		select {
		case <-flushCh:
			flushStats()
		case st, ok := <-t.stCh:
			if !ok {
				flushStats() // Final flush
				return
			}
			if st.metric == "c" {
				if _, ok := counts[st.name]; !ok {
					counts[st.name] = 0
				}
				counts[st.name] += int64(st.value)
			} else if st.metric == "g" {
				gauges[st.name] = st.value
			} else if st.metric == "ms" {
				if _, ok := timers[st.name]; !ok {
					timers[st.name] = make([]float64, 4)
				}
				timers[st.name] = append(timers[st.name], st.value)
			} else {
				log.Printf("statWorker(): invalid metric type: %q, ignoring.", st.metric)
			}
		}
	}
}