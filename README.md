# gRPC custom DNS resolver

gRPC custom DNS resolver implementation based on the original DNS resolver.
https://github.com/grpc/grpc-go/blob/master/internal/resolver/dns/dns_resolver.go

# Differences

* resolution rate is set to 1s delay (original was 30s)
* no exponential backoff when server returns an error
* remove TXT and SRV record support to reduce the queries
