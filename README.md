# sabre

⚔️ **sabre** is a minimal, high-performance JSON-RPC load balancer for Ethereum nodes. It's built with one goal:

1. **get data from multiple RPC providers as fast as possible with as little ongoing maintenance or thought.**

## Installation

To build from source, you can use the following commands:

```bash
git clone https://github.com/terminally-online/sabre.git
cd sabre
go build -o sabre
```

## Quickstart

Before you run ⚔️ **sabre**, we need to setup your RPC providers.

1. Copy `config.toml` to `config.local.toml`
2. Edit `config.local.toml` to have your RPC providers defined at the bottom.

With your `config.local.toml` file ready, you can run ⚔️ **sabre** with:

```bash
go run main.go -c config.local.toml
```

or you can see all the available flags with:

```bash
go run main.go --help
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

Setting it to the name or chain id has worked well for me. Then you can create a simple function like:

```go
func NewClient(ctx context.Context, chain *chains.Chain, scheme string) (*Client, error) {
	rpcURL := fmt.Sprintf("%s://%s/%d", scheme, common.Config.RpcUri, chain.Node.NetworkId)
	return ethclient.DialContext(ctx, rpcURL)
}
```

Only 4 lines of code needed to get a multichain client that has built in retries, batching, and failover.

## Development

### Running Tests

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -v -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out

# Run specific test packages
go test ./internal/backend -v
go test ./internal/router -v
```

### CI/CD

This project uses GitHub Actions for continuous integration. The following workflows are configured:

- **Tests** (`.github/workflows/test.yml`): Runs tests on every push and pull request
- **CI** (`.github/workflows/ci.yml`): Comprehensive CI pipeline including linting, static analysis, and multi-platform builds
- **Security** (`.github/workflows/security.yml`): Security scanning including vulnerability checks and secret detection

All workflows run automatically on:

- Push to `main` or `master` branches
- Pull requests to `main` or `master` branches
- Security scans also run weekly via cron schedule

### Code Quality

The CI pipeline includes:

- **Linting**: Uses `golint` to check code style
- **Static Analysis**: Uses `staticcheck` for advanced static analysis
- **Race Detection**: Tests run with `-race` flag to detect race conditions
- **Security Scanning**: Uses `govulncheck` and `gosec` for vulnerability detection
- **Secret Detection**: Uses `trufflehog` to detect accidentally committed secrets
