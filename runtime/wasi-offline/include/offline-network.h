#ifndef PHP_WASM_OFFLINE_NETWORK_H
#define PHP_WASM_OFFLINE_NETWORK_H
#include <sys/socket.h>
/* The SDK declares AF_UNIX but Preview1 has no sockaddr_un path field. */
#undef AF_UNIX
#ifndef SO_KEEPALIVE
#define SO_KEEPALIVE 9
#endif
#ifndef SOL_IP
#define SOL_IP 0
#endif
int socket(int, int, int);
int connect(int, const struct sockaddr *, socklen_t);
int bind(int, const struct sockaddr *, socklen_t);
int listen(int, int);
int getsockname(int, struct sockaddr *, socklen_t *);
int getpeername(int, struct sockaddr *, socklen_t *);
int setsockopt(int, int, int, const void *, socklen_t);
#endif
