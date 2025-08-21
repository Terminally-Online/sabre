# sabre

⚔️ **sabre** is a minimal, high-performance JSON-RPC load balancer for Ethereum nodes. It's built with one goal:

1. **get data from multiple RPC providers as fast as possible with as little ongoing maintenance or thought.**

## Installation

### From Source

To build from source, you can use the following commands:

```bash
git clone https://github.com/terminally-online/sabre.git
cd sabre
go build -o sabre ./cmd/sabre
```

### From Releases

Download the latest release for your platform from the [releases page](https://github.com/terminally-online/sabre/releases).

### Development

For development, you can run directly with:

```bash
go run ./cmd/sabre
```

## Quickstart

Before you run ⚔️ **sabre**, we need to setup your RPC providers.

1. Copy `config.toml` to `config.local.toml`
2. Edit `config.local.toml` to have your RPC providers defined at the bottom.

With your `config.local.toml` file ready, you can run ⚔️ **sabre** with:

```bash
go run ./cmd/sabre -c config.local.toml
```

or you can see all the available flags with:

```bash
go run ./cmd/sabre --help
```

You can also check the version with:

```bash
go run ./cmd/sabre --version
```

## Usage

With ⚔️ **sabre** running, you can can submit JSON-RPC requests to the following endpoints:

- `http://localhost{config.listen}/{chain_domain}`
- `ws://localhost{config.listen}/{chain_domain}`

The value of `{chain_domain}` is the value you set when configuring the urls of a provider like:

```toml
[providers.ethereum]
url = "..."
```

When it comes to integrating your client you can create a simple function like:

```go
func NewClient(ctx context.Context, chain *chains.Chain, scheme string) (*Client, error) {
	rpcURL := fmt.Sprintf("%s://%s/%d", scheme, common.Config.RpcUri, chain.Node.NetworkId)
	return ethclient.DialContext(ctx, rpcURL)
}
```

Only 4 lines of code needed to get a multichain client that has built in retries, batching, and failover.

## Automated Builds

This project uses GitHub Actions for automated builds and releases:

- **Builds**: Every push to `main` triggers builds for Linux, macOS, and Windows
- **Releases**: When you create a tag starting with `v` (e.g., `v1.0.0`), it automatically creates a GitHub release with binaries for all platforms
- **Testing**: All builds include comprehensive testing with race condition detection

To create a new release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The GitHub Action will automatically build and release binaries for all supported platforms.
