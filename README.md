# Sidecar

[OneBusAway](https://onebusaway.org) Sidecar server reference implementation written in Golang. 

## About

OneBusAway *sidecar services*: the region-scoped HTTP APIs that the OneBusAway mobile apps use for features the core OneBusAway REST API server does not provide — service alerts, tripdeparture alarms, iOS Live Activities, rider surveys, ghost bus reports, push notification registration, weather, vehicle search, and donations.

## Specification

See [specification.md](specification/specification.md) for the complete specification of the Sidecar server and [openapi.yaml](specification/openapi.yaml) for the OpenAPI spec.

## Reference Implementation

The reference implementation is a full implementation of the sidecar services spec in Golang. 

### Caveats

The sidecar server requires a few other services to function properly: a job queue, a database (probably PostgreSQL), and a functioning instance of the [gorush](https://github.com/appleboy/gorush) push notification server to actually send push notifications.

## Development

Requires Go 1.26+ (`mise install` will set it up) and [golangci-lint](https://golangci-lint.run) 2.12+.

```sh
make tools   # install pinned dev tooling
make check   # fmt-check + vet + lint + test — everything CI runs
make run     # build and run the server
make help    # list all targets
```

Run `make check` before opening a pull request.

## License

This project is made available under the terms of the Apache 2.0 license. (c) Open Transit Software Foundation.