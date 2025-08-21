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
go run ./cmd/sabre/main.go
```

## Quickstart

Before you run ⚔️ **sabre**, we need to setup your RPC providers.

1. Copy `config.toml` to `config.local.toml`
2. Edit `config.local.toml` to have your RPC providers defined at the bottom.

With your `config.local.toml` file ready, you can run ⚔️ **sabre** with:

```bash
./sabre -c config.local.toml
```

or you can see all the available flags with:

```bash
./sabre --help
```

You can also check the version with:

```bash
./sabre --version
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
