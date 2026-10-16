/* Preview1 cannot create outgoing sockets. These adapters let libcurl retain
 * its real transfer/option APIs and report ordinary connection failures.
 * They never return a descriptor or introduce a network host import. */
#include "offline-network.h"
#include "netdb.h"
#include <errno.h>
#include <stddef.h>
int socket(int family, int type, int protocol) { errno = ENOSYS; return -1; }
int connect(int fd, const struct sockaddr *addr, socklen_t len) { errno = ENOSYS; return -1; }
int bind(int fd, const struct sockaddr *addr, socklen_t len) { errno = ENOSYS; return -1; }
int listen(int fd, int backlog) { errno = ENOSYS; return -1; }
int getsockname(int fd, struct sockaddr *addr, socklen_t *len) { errno = ENOSYS; return -1; }
int getpeername(int fd, struct sockaddr *addr, socklen_t *len) { errno = ENOSYS; return -1; }
int setsockopt(int fd, int level, int option, const void *value, socklen_t len) { errno = ENOSYS; return -1; }
int getaddrinfo(const char *host, const char *service, const struct addrinfo *hints, struct addrinfo **result)
{
    if (result) *result = NULL;
    return EAI_FAIL;
}
void freeaddrinfo(struct addrinfo *result) {}
const char *gai_strerror(int error) { return "Network unavailable in WASI Preview1"; }
struct hostent *gethostbyname(const char *name) { errno = ENOSYS; return NULL; }
