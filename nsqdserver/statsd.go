package nsqdserver

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"time"

	"github.com/absolute8511/nsq/internal/statsd"
	"github.com/absolute8511/nsq/nsqd"
)

type Uint64Slice []uint64

func (s Uint64Slice) Len() int {
	return len(s)
}

func (s Uint64Slice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Uint64Slice) Less(i, j int) bool {
	return s[i] < s[j]
}

func (n *NsqdServer) statsdLoop() {
	var lastMemStats runtime.MemStats
	var lastStats []nsqd.TopicStats
	opts := n.ctx.getOpts()
	ticker := time.NewTicker(opts.StatsdInterval)
	for {
		select {
		case <-n.exitChan:
			goto exit
		case <-ticker.C:
			n.ctx.nsqd.UpdateTopicHistoryStats()
			client := statsd.NewClient(opts.StatsdAddress, opts.StatsdPrefix)
			err := client.CreateSocket(opts.StatsdProtocol)
			if err != nil {
				nsqd.NsqLogger().Logf("failed to create %v socket to statsd(%s)", opts.StatsdProtocol, client)
				continue
			}

			nsqd.NsqLogger().LogDebugf("STATSD: pushing stats to %s, using prefix: %v", client, opts.StatsdPrefix)

			stats := n.ctx.nsqd.GetStats(false)
			for _, topic := range stats {
				// try to find the topic in the last collection
				lastTopic := nsqd.TopicStats{}
				for _, checkTopic := range lastStats {
					if topic.TopicName == checkTopic.TopicName {
						lastTopic = checkTopic
						break
					}
				}
				statdName := topic.StatsdName
				if topic.IsMultiOrdered {
					if !topic.IsLeader {
						continue
					}
				}
				diff := topic.MessageCount - lastTopic.MessageCount
				stat := fmt.Sprintf("topic.%s.message_count", statdName)
				err := client.Incr(stat, int64(diff))
				if err != nil {
					nsqd.NsqLogger().Logf("STATSD: pushing stats failed: %v", err)
					break
				}

				for _, item := range topic.E2eProcessingLatency.Percentiles {
					stat = fmt.Sprintf("topic.%s.e2e_processing_latency_%.0f", statdName, item["quantile"]*100.0)
					// We can cast the value to int64 since a value of 1 is the
					// minimum resolution we will have, so there is no loss of
					// accuracy
					client.Gauge(stat, int64(item["value"]))
				}

				for _, channel := range topic.Channels {
					// try to find the channel in the last collection
					lastChannel := nsqd.ChannelStats{}
					for _, checkChannel := range lastTopic.Channels {
						if channel.ChannelName == checkChannel.ChannelName {
							lastChannel = checkChannel
							break
						}
					}
					diff := channel.MessageCount - lastChannel.MessageCount
					stat := fmt.Sprintf("topic.%s.channel.%s.message_count", statdName, channel.ChannelName)
					client.Incr(stat, int64(diff))

					stat = fmt.Sprintf("topic.%s.channel.%s.depth", statdName, channel.ChannelName)
					client.Gauge(stat, channel.Depth)

					stat = fmt.Sprintf("topic.%s.channel.%s.backend_depth", statdName, channel.ChannelName)
					client.Gauge(stat, channel.BackendDepth)

					stat = fmt.Sprintf("topic.%s.channel.%s.in_flight_count", statdName, channel.ChannelName)
					client.Gauge(stat, int64(channel.InFlightCount))

					stat = fmt.Sprintf("topic.%s.channel.%s.deferred_count", statdName, channel.ChannelName)
					client.Gauge(stat, int64(channel.DeferredCount))

					diff = channel.RequeueCount - lastChannel.RequeueCount
					stat = fmt.Sprintf("topic.%s.channel.%s.requeue_count", statdName, channel.ChannelName)
					client.Incr(stat, int64(diff))

					diff = channel.TimeoutCount - lastChannel.TimeoutCount
					stat = fmt.Sprintf("topic.%s.channel.%s.timeout_count", statdName, channel.ChannelName)
					client.Incr(stat, int64(diff))

					stat = fmt.Sprintf("topic.%s.channel.%s.clients", statdName, channel.ChannelName)
					client.Gauge(stat, int64(len(channel.Clients)))

					for _, item := range channel.E2eProcessingLatency.Percentiles {
						stat = fmt.Sprintf("topic.%s.channel.%s.e2e_processing_latency_%.0f", statdName, channel.ChannelName, item["quantile"]*100.0)
						client.Gauge(stat, int64(item["value"]))
					}
				}
			}
			lastStats = stats

			if opts.StatsdMemStats {
				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)

				// sort the GC pause array
				length := len(memStats.PauseNs)
				if int(memStats.NumGC) < length {
					length = int(memStats.NumGC)
				}
				gcPauses := make(Uint64Slice, length)
				copy(gcPauses, memStats.PauseNs[:length])
				sort.Sort(gcPauses)

				client.Gauge("mem.heap_objects", int64(memStats.HeapObjects))
				client.Gauge("mem.heap_idle_bytes", int64(memStats.HeapIdle))
				client.Gauge("mem.heap_in_use_bytes", int64(memStats.HeapInuse))
				client.Gauge("mem.heap_released_bytes", int64(memStats.HeapReleased))
				client.Gauge("mem.gc_pause_usec_100", int64(percentile(100.0, gcPauses, len(gcPauses))/1000))
				client.Gauge("mem.gc_pause_usec_99", int64(percentile(99.0, gcPauses, len(gcPauses))/1000))
				client.Gauge("mem.gc_pause_usec_95", int64(percentile(95.0, gcPauses, len(gcPauses))/1000))
				client.Gauge("mem.next_gc_bytes", int64(memStats.NextGC))
				err := client.Incr("mem.gc_runs", int64(memStats.NumGC-lastMemStats.NumGC))
				if err != nil {
					nsqd.NsqLogger().Logf("STATSD: pushing stats failed: %v", err)
				}

				lastMemStats = memStats
			}

			client.Close()
		}
	}

exit:
	ticker.Stop()
}

func percentile(perc float64, arr []uint64, length int) uint64 {
	if length == 0 {
		return 0
	}
	indexOfPerc := int(math.Floor(((perc / 100.0) * float64(length)) + 0.5))
	if indexOfPerc >= length {
		indexOfPerc = length - 1
	}
	return arr[indexOfPerc]
}
