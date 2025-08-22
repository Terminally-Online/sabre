# sabre

⚔️ **sabre** is a minimal, high-performance JSON-RPC load balancer for Ethereum nodes. It's built with one goal:

1. **get data from multiple RPC providers as fast as possible with as little ongoing maintenance or thought.**

## Quickstart

Before you run ⚔️ **sabre**, we need to setup your RPC providers by editing `config.toml` with your RPC providers defined at the bottom following the inline config documentation.

With that done, you are ready to build and run ⚔️ **sabre**.

### GitHub

If you would prefer to avoid building from source, ⚔️ **sabre** is available on GitHub Packages. You can download the latest image with:

```bash
docker pull ghcr.io/terminally-online/sabre:latest
```

or a specific version with:

```bash
docker pull ghcr.io/terminally-online/sabre:v1.0.0
```

### Building the Docker Image

To build the image from source, navigate to the root of the repository after cloning and run:

```bash
docker build -t sabre:local .
```

## Using the Docker Image

There are two ways to run the image:

1. Using Docker
2. Using Docker Compose

### Using Plain Docker

To run ⚔️ **sabre** with Docker, run:

```bash
docker run \
  -p 3000:3000 \
  -v ./config.toml:/app/config.toml:ro \
  sabre:local
```

The above command will create a container with `sabre` running and listening on port 3000 along with a volume called `sabre-data` that will store the cache database.

The above command uses `sabre:local` which is the image you built from source. If you want to use the GitHub Container Registry image, you can use `ghcr.io/terminally-online/sabre:latest`.

### Using Docker Compose

```bash
docker-compose up --build
```

⚔️ **sabre** will be exposed on `http://localhost:3000`.

### Development

For development, you can run directly with:

```bash
go run ./cmd/sabre/main.go
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
