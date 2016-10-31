//
// Copyright 2016 Gregory Trubetskoy. All Rights Reserved.
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

package receiver

import (
	"log"
	"math/rand"
	"time"
)

var workerPeriodicFlushSignal = func(periodicFlushCheck chan bool, minCacheDur, maxCacheDur time.Duration) {
	for {
		// Sleep randomly between min and max cache durations (is this wise?)
		i := int(maxCacheDur.Nanoseconds()-minCacheDur.Nanoseconds()) / 1000000
		dur := time.Duration(rand.Intn(i+1))*time.Millisecond + minCacheDur
		time.Sleep(dur)
		periodicFlushCheck <- true
	}
}

var workerPeriodicFlush = func(ident string, dsf dsFlusherBlocking, recent map[int64]bool, dss *dsCache, minCacheDur, maxCacheDur time.Duration, maxPoints int) {
	for dsId, _ := range recent {
		rds := dss.getById(dsId)
		if rds == nil {
			log.Printf("%s: Cannot lookup ds id (%d) to flush (possible if it moved to another node).", ident, dsId)
			delete(recent, dsId)
			continue
		}
		if rds.shouldBeFlushed(maxPoints, minCacheDur, minCacheDur) {
			if debug {
				log.Printf("%s: Requesting (periodic) flush of ds id: %d", ident, rds.Id())
			}
			dsf.flushDs(rds, false)
			delete(recent, rds.Id())
		}
	}
}

var worker = func(wc wController, dsf dsFlusherBlocking, workerCh chan *incomingDpWithDs, dss *dsCache, minCacheDur, maxCacheDur time.Duration, maxPoints int) {
	wc.onEnter()
	defer wc.onExit()

	recent := make(map[int64]bool)

	periodicFlushCheck := make(chan bool)
	go workerPeriodicFlushSignal(periodicFlushCheck, minCacheDur, maxCacheDur)

	log.Printf("  - %s started.", wc.ident())
	wc.onStarted()

	for {
		select {
		case <-periodicFlushCheck:
			workerPeriodicFlush(wc.ident(), dsf, recent, dss, minCacheDur, maxCacheDur, maxPoints)
		case dpds, ok := <-workerCh:
			if !ok {
				return
			}
			rds := dpds.rds // at this point ds has to be already set
			if err := rds.ProcessIncomingDataPoint(dpds.dp.Value, dpds.dp.TimeStamp); err == nil {
				if rds.shouldBeFlushed(maxPoints, minCacheDur, maxCacheDur) {
					// flush just this one ds
					if debug {
						log.Printf("%s: Requesting flush of ds id: %d", wc.ident(), rds.Id())
					}
					dsf.flushDs(rds, false)
					delete(recent, rds.Id())
				} else {
					recent[rds.Id()] = true
				}
			} else {
				log.Printf("%s: dp.process(%s) error: %v", wc.ident(), rds.Name(), err)
			}
		}

	}
}