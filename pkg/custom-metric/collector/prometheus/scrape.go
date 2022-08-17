/*
Copyright 2022 The Katalyst Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prometheus

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/alecthomas/units"
	"github.com/cespare/xxhash"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/expfmt"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/kubewharf/katalyst-core/pkg/custom-metric/store/data"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
)

// those variables define the http-related configurations for
var (
	httpMetricURL    = "http://%v:%v/custom_metric"
	httpAcceptHeader = "application/openmetrics-text;version=1.0.0,application/openmetrics-text;version=0.0.1;q=0.75,text/plain;version=0.0.4;q=0.5,*/*;q=0.1"
	httpUserAgent    = "katalyst/v1alpha1"

	httpBodyLimit    = int64(10 * units.MiB)
	httpBodyExceeded = fmt.Errorf("body size limit exceeded")
)

// ScrapeManager is responsible for scraping logic through http requests
// and each endpoint will have one manager instance for efficiency.
type ScrapeManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	// lastScrapeSize is used to initialize the buffer size using historical length
	sync.Mutex
	storedSeriesMap map[uint64]*data.MetricSeries

	node string
	url  string

	req     *http.Request
	client  *http.Client
	emitter metrics.MetricEmitter
}

func NewScrapeManager(ctx context.Context, client *http.Client, node, url string, emitter metrics.MetricEmitter) (*ScrapeManager, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Accept", httpAcceptHeader)
	req.Header.Add("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", strconv.FormatFloat(60, 'f', -1, 64))

	sCtx, cancel := context.WithCancel(ctx)
	return &ScrapeManager{
		ctx:     sCtx,
		cancel:  cancel,
		req:     req,
		client:  client,
		node:    node,
		url:     url,
		emitter: emitter,

		storedSeriesMap: make(map[uint64]*data.MetricSeries),
	}, nil
}

func (s *ScrapeManager) Start(duration time.Duration) {
	klog.Infof("start scrape manger with url: %v", s.url)
	go wait.Until(func() { s.scrape() }, duration, s.ctx.Done())
}

func (s *ScrapeManager) Stop() {
	klog.Infof("stop scrape manger with url: %v", s.url)
	s.cancel()
}

// HandleMetric handles the in-cached metric, clears those metric if handle successes
// keep them in memory otherwise
func (s *ScrapeManager) HandleMetric(f func(d *data.MetricSeries) error) {
	s.Lock()
	defer s.Unlock()

	unSucceedSeries := make(map[uint64]*data.MetricSeries)
	for key, series := range s.storedSeriesMap {
		if err := f(series); err != nil {
			klog.Errorf("failed to handle series %v: %v", series.Name, err)
			unSucceedSeries[key] = series
		}
	}
	klog.Infof("scrape [%v] total %v, handled %v", s.url, len(s.storedSeriesMap), len(unSucceedSeries))
	s.storedSeriesMap = unSucceedSeries
}

// scrape periodically scrape metric info from prometheus service, and then puts in the given store.
func (s *ScrapeManager) scrape() {
	buf := bytes.NewBuffer([]byte{})

	if err := s.fetch(s.ctx, s.url, buf); err != nil {
		klog.Errorf("fetch contents failed: %v", err)
		return
	}

	klog.V(6).Infof("node %v parseContents size %v", s.node, len(buf.Bytes()))
	mf, err := parseContents(buf)
	if err != nil {
		klog.Errorf("node %v parseContents contents failed: %v", s.node, err)
		return
	}
	klog.Infof("node %v parseContents contents successfully", s.node)

	s.Lock()
	defer s.Unlock()
	// we only cares about metric with valid contents and types
	for _, v := range mf {
		if v == nil || v.Name == nil || len(v.Metric) == 0 || v.Type == nil || *v.Type != dto.MetricType_GAUGE {
			continue
		}

		for _, m := range v.Metric {
			if m == nil || m.Gauge == nil || m.Gauge.Value == nil {
				continue
			}

			labels := parseLabels(m)

			timestamp, ok := parseTimestamp(labels, m)
			if !ok {
				continue
			}

			hash := calculateHash(*v.Name, labels, m)
			if _, ok := s.storedSeriesMap[hash]; ok {
				continue
			}

			s.storedSeriesMap[hash] = &data.MetricSeries{
				Name:   *v.Name,
				Labels: labels,
				Series: []*data.MetricData{
					{
						Data:      int64(*m.Gauge.Value),
						Timestamp: timestamp,
					},
				},
			}
		}
	}
}

// fetch gets contents from prometheus http service.
func (s *ScrapeManager) fetch(ctx context.Context, url string, w io.Writer) error {
	resp, err := s.client.Do(s.req.WithContext(ctx))
	if err != nil {
		return err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP status %s", resp.Status)
	}

	klog.V(6).Infof("url: %v content type: %v", url, resp.Header.Get("Content-Encoding"))
	if resp.Header.Get("Content-Encoding") != "gzip" {
		n, err := io.Copy(w, io.LimitReader(resp.Body, httpBodyLimit))
		if err != nil {
			return err
		}
		if n >= httpBodyLimit {
			return httpBodyExceeded
		}
		return nil
	}

	klog.V(6).Infof("use gzip to parse url: %v", url)
	gzipR, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to init gzipR: %v", err)
	}

	_ = gzipR.Close()
	n, err := io.Copy(w, io.LimitReader(gzipR, httpBodyLimit))
	if err != nil {
		return err
	}
	if n >= httpBodyLimit {
		return httpBodyExceeded
	}

	return nil
}

// parseContents analyses the contents scraped from prometheus http service.
func parseContents(r io.Reader) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	mf, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, err
	}

	return mf, nil
}

// calculateHash makes sure that we won't store duplicated metric contents
func calculateHash(name string, labels map[string]string, metric *dto.Metric) uint64 {
	b := make([]byte, 0, 1024)
	b = append(b, name...)

	for k, v := range labels {
		b = append(b, '\xff')
		b = append(b, k...)
		b = append(b, '\xff')
		b = append(b, v...)
	}

	if metric.TimestampMs != nil {
		b = append(b, '\xff')
		b = append(b, fmt.Sprintf("%v", *metric.TimestampMs)...)
	}

	return xxhash.Sum64(b)

}

// parseLabels returns labels in key-value formats
func parseLabels(metric *dto.Metric) map[string]string {
	res := make(map[string]string)
	if metric.Label != nil {
		for _, v := range metric.Label {
			if v != nil && v.Name != nil && v.Value != nil {
				res[*v.Name] = *v.Value
			}
		}
	}
	return res
}

// parseTimestamp is an adaptive logic for openTelemetry since its
// default prometheus exporter doesn't enable the ability of timestamp
// like the standard format. but the TimestampMs fields is always prior
// to label-parsed results.
func parseTimestamp(labels map[string]string, metric *dto.Metric) (int64, bool) {
	if metric.TimestampMs != nil {
		return *metric.TimestampMs, true
	}

	if ts, ok := labels[fmt.Sprintf("%s", data.CustomMetricLabelKeyTimestamp)]; ok {
		i, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			klog.Errorf("invalid ts %s for custom metric", ts)
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func newPrometheusClient() (*http.Client, error) {
	return config.NewClientFromConfig(config.HTTPClientConfig{
		FollowRedirects: true,
	}, "prometheus-collector")
}
