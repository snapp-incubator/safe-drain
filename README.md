# Safe Drain

safe-drain is a simple tool that helps safely evict pods from a Kubernetes node by ensuring they are properly recreated before moving on. It is
designed as a safer alternative to kubectl drain, focusing on controller-managed pods to minimize disruption.

## Features

- Evicts controller-managed pods (Deployments, StatefulSets, DaemonSets, etc.) one by one.
- Waits for each evicted pod to be recreated and ready before continuing.
- Helps perform safe node draining during maintenance or scaling operations.
- Simple CLI with minimal configuration.

## Installation

Clone the repository and build from source:

```bash
git clone https://github.com/<your-username>/safe-drain.git
cd safe-drain
go build -o safe-drain
```

Or download a pre-built binary from Releases.

## Usage

```bash
safe-drain -kubeconfig ~/.kube/config -node <node-name>
```

## Options

```text
-kubeconfig string
    absolute path to the kubeconfig file
-node string
    The name of the node to drain and verify.
-timeout duration
    Timeout for waiting for all pods to be replaced. (default 10m0s)
```

## Example

Drain a node named worker-1 using a custom kubeconfig:

```bash
safe-drain -kubeconfig ~/.kube/my-cluster.yaml -node worker-1
```

## Release Process

1. Create a new tag on the main branch
2. Create a new release on GitHub with the tag
3. GitHub Actions will automatically build and publish the binaries

## Contributing

Contributions are welcome! Feel free to open issues or submit pull requests to improve functionality.
