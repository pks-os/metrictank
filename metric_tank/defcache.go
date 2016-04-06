package main

import (
	"github.com/grafana/grafana/pkg/log"
	"github.com/raintank/raintank-metric/metric_tank/idx"
	"github.com/raintank/raintank-metric/metricdef"
	"github.com/raintank/raintank-metric/schema"
	"sync"
	"time"
)

// TODO call Prune every 5min or so

// design notes:
// MT pulls in all definitions when it starts up.
// those "old" ones + whatever it sees as inputs from the metrics queue
// is enough for it to always know the complete current state
// nothing should update ES "behind its back", so we never need to pull
// from ES other then at startup.
// but other MT instances may update ES while we are down, so ES is a good
// place to pull from, until the performance is demonstrably too slow.
// there are some vectors here for race conditions but we can work those out
// later, perhaps when tacking the multiple-intervals work

type DefCache struct {
	sync.RWMutex
	defs      []schema.MetricDefinition
	ById      map[string]idx.MetricID // by hashed id. we store uints, not pointers, to lower GC workload.
	ByKey     *idx.Idx                // by graphite key aka "Name" in the def to support graphite native api. this index is experimental and may be removed in the future
	defsStore metricdef.Defs
}

func NewDefCache(defsStore metricdef.Defs) *DefCache {
	d := &DefCache{
		ById:      make(map[string]idx.MetricID),
		ByKey:     idx.New(),
		defsStore: defsStore,
	}
	d.Backfill()
	return d
}

// backfill definitions from ES
// in theory, there is a race between defs from ES and from nsq
// in practice, it doesn't matter: you're only supposed to query MT
// after a while, after which the defs surely have stabilized.
func (dc *DefCache) Backfill() {
	total := 0
	add := func(met []*schema.MetricDefinition) {
		if len(met) > 0 {
			total += len(met)
			dc.Lock()
			for _, def := range met {
				id := dc.ByKey.GetOrAdd(def.OrgId, def.Name) // gets id auto assigned from 0 and onwards
				dc.ByKey.AddRef(def.OrgId, id)
				dc.ById[def.Id] = id
				dc.defs = append(dc.defs, *def) // which maps 1:1 with pos in this array
			}
			dc.Unlock()
		}
	}
	met, scroll_id, err := dc.defsStore.GetMetrics("")
	if err != nil {
		log.Error(3, "Could not backfill from ES: %s", err)
		return
	}
	add(met)
	for scroll_id != "" {
		met, scroll_id, err = dc.defsStore.GetMetrics(scroll_id)
		if err != nil {
			log.Error(3, "Could not backfill from ES: %s", err)
			return
		}
		add(met)
	}
	log.Debug("backfilled %d metric definitions", total)
}

func (dc *DefCache) Add(metric *schema.MetricData) {
	dc.Lock()
	id, ok := dc.ById[metric.Id]
	dc.Unlock()
	if ok {
		//If the time diff between this datapoint and the lastUpdate
		// time of the metricDef is greater than 6hours, update the metricDef.
		dc.Lock()
		mdef := dc.defs[id]
		dc.Unlock()
		if mdef.LastUpdate < metric.Time-21600 {
			mdef = *schema.MetricDefinitionFromMetricData(metric)
			dc.addToES(&mdef)
			dc.Lock()
			dc.defs[id] = mdef
			dc.Unlock()
		}
	} else {
		mdef := *schema.MetricDefinitionFromMetricData(metric)
		dc.addToES(&mdef)
		dc.Lock()
		id := dc.ByKey.GetOrAdd(mdef.OrgId, mdef.Name)
		dc.ByKey.AddRef(mdef.OrgId, id)
		dc.ById[mdef.Id] = id
		dc.defs = append(dc.defs, mdef)
		dc.Unlock()
	}
}

func (dc *DefCache) addToES(mdef *schema.MetricDefinition) {
	pre := time.Now()
	err := dc.defsStore.IndexMetric(mdef)
	// NOTE: indexing to ES is done asyncrounously using the bulkAPI.
	// so an error here is just an error adding the document to the
	// bulkAPI buffer.
	if err != nil {
		log.Error(3, "couldn't index to ES %s: %s", mdef.Id, err)
		metricsToEsFail.Inc(1)
	} else {
		metricsToEsOK.Inc(1)
	}
	esPutDuration.Value(time.Now().Sub(pre))
}

// note: the defcache is clearly not a perfect all-knowning entity, it just knows the last interval of metrics seen since program start
// and we assume we can use that interval through history.
// TODO: no support for interval changes, missing datablocks, ...
func (dc *DefCache) Get(id string) (*schema.MetricDefinition, bool) {
	dc.RLock()
	i, ok := dc.ById[id]
	var def *schema.MetricDefinition
	if ok {
		def = &dc.defs[i]
	}
	dc.RUnlock()
	return def, ok
}

func (dc *DefCache) Find(org int, key string) []*schema.MetricDefinition {
	globs := dc.ByKey.Match(org, key)
	defs := make([]*schema.MetricDefinition, len(globs))
	dc.RLock()
	for i, g := range globs {
		defs[i] = &dc.defs[g.Id]
	}
	dc.RUnlock()
	return defs
}

func (dc *DefCache) List(org int) []*schema.MetricDefinition {
	list := dc.ByKey.List(org)
	out := make([]*schema.MetricDefinition, len(list))
	dc.RLock()
	for i, id := range list {
		out[i] = &dc.defs[id]
	}
	dc.RUnlock()
	return out
}
