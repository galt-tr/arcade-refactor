package kafka

import "github.com/IBM/sarama"

// NewProducerWithSync creates a Producer with a custom SyncProducer.
// Intended for testing — allows injecting mock producers.
func NewProducerWithSync(sp sarama.SyncProducer) *Producer {
	return &Producer{syncProducer: sp}
}
