package kafka

const (
	TopicBlock          = "arcade.block"
	TopicStump          = "arcade.stump"
	TopicBlockProcessed = "arcade.block_processed"
	TopicTransaction    = "arcade.transaction"
	TopicPropagation    = "arcade.propagation"

	// Dead-letter queue topics
	TopicBlockDLQ          = "arcade.block.dlq"
	TopicStumpDLQ          = "arcade.stump.dlq"
	TopicBlockProcessedDLQ = "arcade.block_processed.dlq"
	TopicTransactionDLQ    = "arcade.transaction.dlq"
	TopicPropagationDLQ    = "arcade.propagation.dlq"
)

// AllTopics returns all primary topics.
func AllTopics() []string {
	return []string{
		TopicBlock,
		TopicStump,
		TopicBlockProcessed,
		TopicTransaction,
		TopicPropagation,
	}
}

// DLQTopic returns the dead-letter topic name for a given primary topic.
func DLQTopic(topic string) string {
	return topic + ".dlq"
}
