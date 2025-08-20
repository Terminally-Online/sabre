# rapier

⚔️ **rapier** is a minimal, high-performance JSON-RPC load balancer for Ethereum nodes. It’s built with one goal:

1. **send requests as fast as possible across multiple RPC providers with as little ongoing maintenance or thought.**

## Installation

To build from source, you can use the following commands:

```bash
git clone https://github.com/terminally-online/rapier.git
cd rapier
go build -o rapier
```

or you can download a pre-built binary from the [releases](https://github.com/terminally-online/rapier/releases) page.

## Usage

To run ⚔️ **rapier**, you can use the following command:

```bash
go run main.go -c config.local.toml
```

or you see all the available options with:

```bash
go run main.go --help
```

## Configuration

⚔️ **rapier** is configured via a `config.toml` file that is pre-configured with the most common sane defaults.

The configuration is organized into logical sections:

- **[rapier]**: Core server settings (listen address, retry attempts)
- **[performance]**: Load balancing, connection pooling, HTTP/2, and compression settings
- **[batch]**: Request batching configuration for improved throughput
- **[health]**: Health checking and backend monitoring settings
- **[cache]**: Caching behavior, storage configuration, cleanup settings, and re-org protection

Out-of-box, the only thing **you need to update** is your **RPC provider table(s)**. Though, if you prefer, you can change the values of any and all fields in the configuration file as well as utilize a `.env` file to set the configuration dynamically based on your environment.

## Monitoring

Rapier provides a simple health check endpoint at `/_health` that returns:

- **200 OK**: All chains have at least one healthy backend
- **503 Service Unavailable**: One or more chains have no healthy backends
- **JSON response**: Detailed status of all backends including latency and failure streaks

This endpoint combines both health checking and readiness logic, making it perfect for load balancer health checks and monitoring systems.

## Re-org Protection

Rapier includes built-in protection against blockchain re-orgs to prevent serving stale data. Re-org detection is **automatically integrated with health monitoring** and provides:

- **Chain-Aware Detection**: Each chain's re-orgs are tracked independently
- **Multi-Method Detection**: Uses three complementary detection methods:
  1. **Block Hash Changes**: Detects when the same block number has a different hash
  2. **Historical Inconsistencies**: Identifies gaps or inconsistencies in the chain
  3. **Parent-Child Validation**: Validates block parent-child relationships
- **Automatic Detection**: Re-orgs are detected during health checks using the same interval
- **Block Hash Tracking**: Store block numbers and hashes with cached data per chain
- **Smart Invalidation**: Automatically clear affected cache entries when re-orgs are detected
- **Conservative Caching**: Use shorter TTLs for "latest" data to reduce re-org impact
- **Memory Efficient**: Only track recent blocks within the configured depth per chain

This is especially important for:

- High-frequency trading applications
- Time-sensitive DeFi operations
- Multi-chain applications
- Any application requiring data consistency

**No additional configuration needed** - re-org protection is automatically enabled when health monitoring is enabled. The `max_reorg_depth` setting in the `[cache]` section controls how many recent blocks to track per chain.
