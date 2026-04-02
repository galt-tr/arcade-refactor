package p2p_client

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
)

// BlockNotification is published to Kafka when a new block is seen.
type BlockNotification struct {
	BlockHash    string `json:"block_hash"`
	Height       uint64 `json:"height"`
	PreviousHash string `json:"previous_hash"`
	Timestamp    int64  `json:"timestamp"`
}

type Client struct {
	cfg      *config.Config
	logger   *zap.Logger
	producer *kafka.Producer

	// dedup tracks recently seen block hashes to avoid duplicate Kafka messages.
	seen   map[string]time.Time
	seenMu sync.Mutex
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer) *Client {
	return &Client{
		cfg:      cfg,
		logger:   logger.Named("p2p-client"),
		producer: producer,
		seen:     make(map[string]time.Time),
	}
}

func (c *Client) Name() string { return "p2p-client" }

func (c *Client) Start(ctx context.Context) error {
	c.logger.Info("starting P2P client", zap.Strings("seeds", c.cfg.P2P.Seeds))

	// Start dedup cleanup goroutine
	go c.cleanupSeen(ctx)

	// Connect and listen for blocks.
	// This is where the Teranode libp2p client would be integrated.
	// For now, this is the hook point — the actual libp2p integration
	// depends on the go-teranode-p2p-client and go-p2p-message-bus packages.
	return c.listenForBlocks(ctx)
}

func (c *Client) Stop() error {
	c.logger.Info("stopping P2P client")
	return nil
}

// listenForBlocks connects to the P2P network and processes block announcements.
// This method integrates with the Teranode libp2p client.
func (c *Client) listenForBlocks(ctx context.Context) error {
	// TODO: Integrate with go-teranode-p2p-client
	// The pattern is:
	//   1. Create a p2p client with configured seeds
	//   2. Subscribe to block announcements via the message bus
	//   3. For each block announcement, call c.onBlock(...)
	//
	// Reconnection is handled by reconnectWithBackoff on connection loss.

	<-ctx.Done()
	return nil
}

// onBlock handles a new block announcement from the P2P network.
func (c *Client) onBlock(blockHash string, height uint64, prevHash string) {
	if c.isDuplicate(blockHash) {
		c.logger.Debug("duplicate block, skipping", zap.String("block_hash", blockHash))
		return
	}

	notification := BlockNotification{
		BlockHash:    blockHash,
		Height:       height,
		PreviousHash: prevHash,
		Timestamp:    time.Now().UnixMilli(),
	}

	if err := c.producer.Send(kafka.TopicBlock, blockHash, notification); err != nil {
		c.logger.Error("failed to publish block notification",
			zap.String("block_hash", blockHash),
			zap.Error(err),
		)
		return
	}

	c.logger.Info("block notification published",
		zap.String("block_hash", blockHash),
		zap.Uint64("height", height),
	)
}

// isDuplicate returns true if this block hash was already processed recently.
func (c *Client) isDuplicate(blockHash string) bool {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	if _, ok := c.seen[blockHash]; ok {
		return true
	}
	c.seen[blockHash] = time.Now()
	return false
}

// cleanupSeen removes old entries from the dedup map.
func (c *Client) cleanupSeen(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.seenMu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for hash, t := range c.seen {
				if t.Before(cutoff) {
					delete(c.seen, hash)
				}
			}
			c.seenMu.Unlock()
		}
	}
}

// reconnectWithBackoff attempts to reconnect to the P2P network with exponential backoff.
func (c *Client) reconnectWithBackoff(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 2 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		c.logger.Info("attempting P2P reconnection", zap.Duration("backoff", backoff))

		// TODO: Actual reconnection logic with go-teranode-p2p-client
		// If connection succeeds, return nil
		// If fails, increase backoff and retry

		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
