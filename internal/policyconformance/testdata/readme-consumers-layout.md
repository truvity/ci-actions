# widget

A small component that does one thing.

| ships | where |
| -- | -- |
| the `widget` chart | an OCI registry |

## Who it is for

Teams that run on a Kubernetes cluster and want a widget. It does not install
the cluster.

## The model

A widget has a name and a size. The chart renders one Deployment per widget.

## Install and a worked example

```sh
helm install widget oci://registry.example.com/charts/widget --version 1.1.0
```

```yaml
name: example
size: 3
```

## Consumers

| consumer | what it uses |
| -- | -- |
| [example/shop](https://example.invalid/example/shop) | the chart |

## Neighbours

- [example/gadget](https://example.invalid/example/gadget): renders gadgets.

## Documentation

- [docs/reference.md](docs/reference.md)

## The rule that makes this repository public

Mechanism only. `hack/leak-canary.sh` enforces it.

## Status

Released as v1.1.0.

## Development

`devbox shell`, then `just check`.

## Releasing

Auto-release is armed for patches. Minors and majors are manual.

## Licence

MIT.
