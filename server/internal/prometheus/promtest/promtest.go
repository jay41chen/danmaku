// 測試專用：讀出 metric 目前的值，函式名稱跟 prometheus package 的寫入函式一一對應
// 用 metric 名稱從 default registry 讀（子 package 看不到 prometheus package 未匯出的變數）
// 名稱寫錯會讀到 0：比 before/after 差值的斷言會失敗，所以不用另外對名稱；但「== 0」的斷言抓不到，要靠其他斷言非 0 值的測試
// 換 telemetry 時跟 prometheus package 一起改，測試呼叫端不用動
package promtest

import (
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// 找出名稱是 name、label 完全符合 labels 的那一條；還沒被寫過（不存在）就回 nil，呼叫端當 0
func find(name string, labels map[string]string) *dto.Metric {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		panic(fmt.Sprintf("promtest: gather: %v", err)) // 只在測試用，讀不到就是測試環境壞了
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				return m
			}
		}
	}
	return nil
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, p := range pairs {
		if v, ok := want[p.GetName()]; !ok || v != p.GetValue() {
			return false
		}
	}
	return true
}

// http_requests_total {method, path, status_code}
func HTTPRequestCount(method, path string, status int) float64 {
	return find("http_requests_total", map[string]string{
		"method": method, "path": path, "status_code": strconv.Itoa(status),
	}).GetCounter().GetValue()
}

// http_request_duration_seconds {method, path} 的 histogram sample 總數
func HTTPRequestDurationCount(method, path string) uint64 {
	return find("http_request_duration_seconds", map[string]string{
		"method": method, "path": path,
	}).GetHistogram().GetSampleCount()
}

// rate_limited_total {rule}
func RateLimitedCount(rule string) float64 {
	return find("rate_limited_total", map[string]string{"rule": rule}).GetCounter().GetValue()
}

// redis_publish_failed_total
func PublishFailedCount() float64 {
	return find("redis_publish_failed_total", nil).GetCounter().GetValue()
}

// websocket_connections_active（gauge）
func WSConnectionsActive() float64 {
	return find("websocket_connections_active", nil).GetGauge().GetValue()
}

// websocket_connections_total
func WSConnectionsTotal() float64 {
	return find("websocket_connections_total", nil).GetCounter().GetValue()
}

// websocket_viewers {auth=member}（gauge）
func MemberViewers() float64 {
	return find("websocket_viewers", map[string]string{"auth": "member"}).GetGauge().GetValue()
}

// websocket_resync_sent_total
func WSResyncCount() float64 {
	return find("websocket_resync_sent_total", nil).GetCounter().GetValue()
}

// redis_pubsub_reconfirm_total
func PubSubReconfirmCount() float64 {
	return find("redis_pubsub_reconfirm_total", nil).GetCounter().GetValue()
}

// redis_pubsub_resubscribe_duration_seconds 的 histogram sample 總數
func PubSubResubscribeDurationCount() uint64 {
	return find("redis_pubsub_resubscribe_duration_seconds", nil).GetHistogram().GetSampleCount()
}
