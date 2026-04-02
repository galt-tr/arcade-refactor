# High Level Overview
Arcade instances register transactions with a merkle-service server. This registration includes a TXID and a callback URL. The callback URL points to a defined endpoint on the arcade instance where we can update the state of the transaction and store a merkle proof when it is included in a block.
See [swimlane diagram](./diagram.png) for more details on how the integration with merkle service works.

# Terms
- [BUMP (Bitcoin Unified Merkle Path)](https://github.com/bitcoin-sv/BRCs/blob/master/transactions/0074.md#abstract) is for full block merkle path for a given transaction
- STUMP (Subtree Unified Merkle Path) follows the same format as BUMP but is a merkle path for a transaction in a given subtree

# Relevant Projects
- [Existing Arcade Project](https://github.com/bsv-blockchain/arcade)
- [Branch of Arcade refactor for merkle service](https://github.com/bsv-blockchain/arcade/tree/merkle-service-integration).
- [Teranode](https://github.com/bsv-blockchain/teranode)
- [Merkle Service](https://github.com/galt-tr/merkle-service)

# Arcade workflow
- arcade stores all blocks (using teranode block definition)
- arcade registers transaction with merkle service
- arcade receives callback when tx is seen in a subtree
- arcade receives callback with STUMP in block
- arcade uses merkle proof of coinbase transaction to build the full bump

# Requirements
- Follow the same daemon/service pattern that [Teranode](https://github.com/bsv-blockchain/teranode) and [Merkle service](https://github.com/galt-tr/merkle-service) uses
- Build with a microservice architecture in mind, but support runing all-in-one like Teranode
- Support scale where blocks contain millions of transactions, subtrees will be thousands of transactions, and arcade may register tens of thousands of transactions per block
- Leverage teranode learnings wherever possible to support high-throughput and massive blocks

# Services
- api-server
  - registers callback endpoint for merkle service
  - callback states:
    - REJECTED
    - SEEN_ON_NETWORK
    - SEEN_MULTIPLE_NODES
    - STUMP
    - BLOCK_PROCESSED
  - GET transactions (get current state and proofs for transactions)
  - POST transactions (broadcast transactions to all configured datahub URLs)
  - all callback states should issue kafka messages when necessary to keep API server very simple and scalable. ALl actual work should happen in other services
- p2p client
  - listens for blocks
  - publishes to kafka
  - teranode libp2p client (see [message bus](https://github.com/bsv-blockchain/go-p2p-message-bus) and [teranode client](https://github.com/bsv-blockchain/go-teranode-p2p-client) for references)
- kafka
  - topics (review these):
    - block (from p2p)
    - stump
    - block_processed
    - transaction
- database
  - store transactions and their respective states
  - must scale really well
  - store stumps as they come in (with proper pruning when a bump is built)
  - stores bumps and udpates transactions with specific merkle proofs.
- block_processor
  - when p2p client sees a block, fetch it from the relevant datahub URL and store full block model
- bump_builder
  - when a BLOCK_PROCESSED callback is issued, find all STUMPs received for that block and build a full BUMP using the coinbase merkle path. Store merkle paths for each transaction
- propagation
  - broadcasts transactions to all configured datahub URLs
- tx_validator
  - validate transaction or set of transactions
  - if valid, send for propagation
  - if invalid, reject

# Assumptions:
- all aerospike calls need to be batched
  - See [blockassembler](https://github.com/bsv-blockchain/teranode/blob/main/services/blockassembly/Client.go#L243) for reference.

