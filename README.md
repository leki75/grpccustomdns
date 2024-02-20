# gRPC custom DNS resolver

gRPC custom DNS resolver implementation based on the original DNS resolver.
https://github.com/grpc/grpc-go/blob/master/internal/resolver/dns/dns_resolver.go

# The problem

gRPC DNS resolver uses 30s delay between DNS resolution requests to reduce the
load on the DNS server. On error it also uses an exponential backoff to.
However, on Kubernetes using round-robin load-balancer this is not appropriate,
a rolling restart of the pods will change all the IPs in the headless service.

This change reduces the timeout to 1s and removes the TXT and SRV record queries
to reduce the number of DNS packets.

# Differences

* resolution rate is set to 1s delay (original was 30s)
* no exponential backoff when server returns an error
* remove TXT and SRV record support to reduce the queries
