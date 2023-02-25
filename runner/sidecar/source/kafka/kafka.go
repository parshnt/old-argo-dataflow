package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	dfv1 "github.com/argoproj-labs/argo-dataflow/api/v1alpha1"
	sharedkafka "github.com/argoproj-labs/argo-dataflow/runner/sidecar/shared/kafka"
	"github.com/argoproj-labs/argo-dataflow/runner/sidecar/source"
	sharedutil "github.com/argoproj-labs/argo-dataflow/shared/util"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/go-logr/logr"
	"github.com/opentracing/opentracing-go"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type kafkaSource struct {
	logger     logr.Logger
	sourceName string
	sourceURN  string
	consumer   *kafka.Consumer
	topic      string
	wg         *sync.WaitGroup
	channels   *sync.Map // map[int32]chan *kafka.Message
	process    source.Process
	totalLag   int64
}

const (
	seconds            = 1000
	pendingUnavailable = math.MinInt32
)

func New(ctx context.Context, secretInterface corev1.SecretInterface, cluster, namespace, pipelineName, stepName, sourceName, sourceURN string, replica int, x dfv1.KafkaSource, process source.Process) (source.Interface, error) {
	logger := sharedutil.NewLogger().WithValues("source", sourceName)
	config, err := sharedkafka.GetConfig(ctx, secretInterface, x.KafkaConfig)
	if err != nil {
		return nil, err
	}
	config["go.logs.channel.enable"] = true
	consumerGroupID := x.GetGroupID(sharedutil.GetSourceUID(cluster, namespace, pipelineName, stepName, sourceName))
	config["group.id"] = consumerGroupID
	config["group.instance.id"] = fmt.Sprintf("%s/%d", consumerGroupID, replica)
	config["heartbeat.interval.ms"] = 3 * seconds
	config["socket.keepalive.enable"] = true
	config["enable.auto.commit"] = false
	config["enable.auto.offset.store"] = false
	config["auto.offset.reset"] = x.GetAutoOffsetReset()
	config["statistics.interval.ms"] = 5 * seconds
	// https://docs.confluent.io/cloud/current/client-apps/optimizing/throughput.html
	config["fetch.min.bytes"] = x.GetFetchMinBytes()
	config["fetch.wait.max.ms"] = x.GetFetchWaitMaxMs()
	// config["go.events.channel.enable"] = true
	// config["max.poll.interval.ms"] = 300 * seconds
	logger.Info("Kafka config", "config", sharedutil.MustJSON(sharedkafka.RedactConfigMap(config)))
	// https://github.com/confluentinc/confluent-kafka-go/blob/master/examples/consumer_example/consumer_example.go
	consumer, err := kafka.NewConsumer(&config)
	if err != nil {
		return nil, err
	}

	go wait.JitterUntilWithContext(ctx, func(context.Context) {
		logger.Info("consuming Kafka logs")
		for e := range consumer.Logs() {
			logger.WithValues("name", e.Name, "tag", e.Tag).Info(e.Message)
		}
	}, 3*time.Second, 1.2, true)

	s := &kafkaSource{
		logger:     logger,
		sourceName: sourceName,
		sourceURN:  sourceURN,
		consumer:   consumer,
		topic:      x.Topic,
		channels:   new(sync.Map), // partition -> messages
		wg:         &sync.WaitGroup{},
		process:    process,
		totalLag:   pendingUnavailable,
	}

	if err = consumer.Subscribe(x.Topic, func(consumer *kafka.Consumer, event kafka.Event) error {
		return s.rebalanced(ctx, event)
	}); err != nil {
		return nil, err
	}

	go wait.JitterUntilWithContext(ctx, s.startPollLoop, 3*time.Second, 1.2, true)

	return s, nil
}

func (s *kafkaSource) processMessage(ctx context.Context, msg *kafka.Message) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, fmt.Sprintf("kafka-source-%s", s.sourceName))

	Id := fmt.Sprintf("%d-%d", msg.TopicPartition.Partition, msg.TopicPartition.Offset)

	// add header value to id
	for _, header := range msg.Headers {
		if header.Value != nil {
			Id += fmt.Sprintf("-%s", string(header.Value))
		}
	}

	defer span.Finish()
	return s.process(
		dfv1.ContextWithMeta(
			ctx,
			dfv1.Meta{
				Source: s.sourceURN,
				ID:     Id,
				Time:   msg.Timestamp.Unix(),
			},
		),
		msg.Value,
	)
}

func (s *kafkaSource) assignedPartition(ctx context.Context, partition int32) {
	logger := s.logger.WithValues("partition", partition)
	if _, ok := s.channels.Load(partition); !ok {
		logger.Info("assigned partition")
		s.channels.Store(partition, make(chan *kafka.Message, 256))
		go wait.JitterUntilWithContext(ctx, func(ctx context.Context) {
			s.consumePartition(ctx, partition)
		}, 3*time.Second, 1.2, true)
	}
}

func (s *kafkaSource) startPollLoop(ctx context.Context) {
	s.logger.Info("starting poll loop")
	for {
		// shutdown will be blocked for the amount of time we specify here
		ev := s.consumer.Poll(5 * 1000)
		select {
		case <-ctx.Done():
			return
		default:
			switch e := ev.(type) {
			case *kafka.Message:
				func() {
					defer func() {
						// Fact 1 - if you send a message on a closed channel, you get a panic.
						// Fact 2 - it is impossible to know if a channel is close in Golang.
						// we need to recover any panic, so we don't pollute the logs
						if r := recover(); r != nil {
							s.logger.Info("recovered from panic while queuing message", "recover", fmt.Sprint(r))
						}
					}()
					v, _ := s.channels.Load(e.TopicPartition.Partition)
					v.(chan *kafka.Message) <- e
				}()
			case *kafka.Stats:
				// https://github.com/edenhill/librdkafka/wiki/Consumer-lag-monitoring
				// https://github.com/confluentinc/confluent-kafka-go/blob/master/examples/stats_example/stats_example.go
				stats := &Stats{}
				if err := json.Unmarshal([]byte(e.String()), stats); err != nil {
					s.logger.Error(err, "failed to unmarshall stats")
				} else {
					s.totalLag = stats.totalLag(s.topic)
				}
			case kafka.Error:
				s.logger.Info("poll error", "error", fmt.Errorf("%v", e))
			case nil:
				// noop
			default:
				s.logger.Info("ignored event", "event", ev)
			}
		}
	}
}

func (s *kafkaSource) Close() error {
	s.logger.Info("closing partition channels")
	s.channels.Range(func(_, v interface{}) bool {
		close(v.(chan *kafka.Message))
		return true
	})
	s.logger.Info("waiting for partition consumers to finish")
	s.wg.Wait()
	s.logger.Info("closing consumer")
	return s.consumer.Close()
}

func (s *kafkaSource) GetPending(context.Context) (uint64, error) {
	if s.totalLag == pendingUnavailable {
		return 0, source.ErrPendingUnavailable
	} else if s.totalLag >= 0 {
		return uint64(s.totalLag), nil
	} else {
		return 0, nil
	}
}

func (s *kafkaSource) rebalanced(ctx context.Context, event kafka.Event) error {
	s.logger.Info("re-balance", "event", event.String())
	switch e := event.(type) {
	case kafka.AssignedPartitions:
		for _, p := range e.Partitions {
			s.assignedPartition(ctx, p.Partition)
		}
	}
	return nil
}

func (s *kafkaSource) consumePartition(ctx context.Context, partition int32) {
	logger := s.logger.WithValues("partition", partition)
	logger.Info("consuming partition")
	s.wg.Add(1)
	var lastUncommitted *kafka.Message
	commitLastUncommitted := func() {
		if lastUncommitted != nil {
			if _, err := s.consumer.CommitMessage(lastUncommitted); err != nil {
				logger.Info("failed to commit message", "offset", lastUncommitted.TopicPartition.Offset, "error", err)
			}
			lastUncommitted = nil
		}
	}
	defer func() {
		logger.Info("committing last uncommitted message")
		commitLastUncommitted()
		logger.Info("done consuming partition")
		s.wg.Done()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		v, _ := s.channels.Load(partition)
		select {
		case <-ticker.C:
			commitLastUncommitted()
		case msg, ok := <-v.(chan *kafka.Message):
			if !ok {
				return
			}
			offset := int64(msg.TopicPartition.Offset)
			logger := logger.WithValues("offset", offset)
			if err := s.processMessage(ctx, msg); err != nil {
				if errors.Is(err, context.Canceled) {
					logger.Info("failed to process message", "err", err.Error())
				} else {
					logger.Error(err, "failed to process message")
				}
			} else {
				lastUncommitted = msg
			}
		}
	}
}
