package main

import (
	"errors"
	"fmt"
	"github.com/grafana/metrictank/stacktest/graphite"
	"github.com/grafana/metrictank/stats"
	log "github.com/sirupsen/logrus"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var (
	httpError   = stats.NewCounter32("parrot.monitoring.error;error=http")
	decodeError = stats.NewCounter32("parrot.monitoring.error;error=decode")
)

type seriesStats struct {
	lastTs uint32
	//the partition currently being checked
	partition int32
	//the number of nans present in the time series
	nans int32
	//the sum of abs(value - ts) across the time series
	deltaSum float64
	//the number of timestamps where value != ts
	numNonMatching int32

	//tracks the last seen non-NaN time stamp (useful for lag
	lastSeen uint32
}

func monitor() {
	for tick := range time.NewTicker(queryInterval).C {

		query := graphite.ExecuteRenderQuery(buildRequest(tick))
		if query.HTTPErr != nil {
			httpError.Inc()
		}
		if query.DecodeErr != nil {
			decodeError.Inc()
		}

		for _, s := range query.Decoded {
			log.Infof("%d - %d", s.Datapoints[0].Ts, s.Datapoints[len(s.Datapoints)-1].Ts)

			partition, err := extractPartition(s.Target)
			if err != nil {
				log.Debug("unable to extract partition", err)
				stats.NewCounter32("parrot.monitoring.error;error=parsePartition").Inc()
				continue
			}
			serStats := seriesStats{}
			serStats.lastTs = s.Datapoints[len(s.Datapoints)-1].Ts

			for _, dp := range s.Datapoints {

				if math.IsNaN(dp.Val) {
					serStats.nans += 1
					continue
				}
				if diff := dp.Val - float64(dp.Ts); diff != 0 {
					log.Debugf("partition=%d dp.Val=%f dp.Ts=%d diff=%f", partition, dp.Val, dp.Ts, diff)
					serStats.lastSeen = dp.Ts
					serStats.deltaSum += diff
					serStats.numNonMatching += 1
				}
			}

			//number of missing values for each series
			stats.NewGauge32(fmt.Sprintf("parrot.monitoring.nancount;partition=%d", serStats.partition)).Set(int(serStats.nans))
			//time since the last value was recorded
			stats.NewGauge32(fmt.Sprintf("parrot.monitoring.lag;partition=%d", serStats.partition)).Set(int(serStats.lastTs - serStats.lastSeen))
			//total amount of drift between expected value and actual values
			stats.NewGauge32(fmt.Sprintf("parrot.monitoring.deltaSum;partition=%d", serStats.partition)).Set(int(serStats.deltaSum))
			//total number of entries where drift occurred
			stats.NewGauge32(fmt.Sprintf("parrot.monitoring.nonMatching;partition=%d", serStats.partition)).Set(int(serStats.numNonMatching))
		}
	}
}

var pattern = regexp.MustCompile(`parrot.testdata.(\d+).generated.\w+`)

func extractPartition(target string) (int32, error) {
	submatch := pattern.FindStringSubmatch(target)
	if len(submatch) < 2 {
		return -1, errors.New(fmt.Sprintf("target [%s] did not match pattern", target))
	}
	partition, err := strconv.Atoi(submatch[1])
	if err != nil {
		return -1, err
	}
	return int32(partition), nil

}

func buildRequest(now time.Time) *http.Request {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/render", gatewayAddress), nil)
	q := req.URL.Query()
	q.Set("target", "parrot.testdata.*.generated.*")
	q.Set("from", strconv.Itoa(int(now.Add(-5*time.Minute).Unix())))
	q.Set("until", strconv.Itoa(int(now.Unix())))
	q.Set("format", "json")
	q.Set("X-Org-Id", strconv.Itoa(orgId))
	req.URL.RawQuery = q.Encode()
	if len(gatewayKey) != 0 {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", gatewayKey))
	}
	return req
}
