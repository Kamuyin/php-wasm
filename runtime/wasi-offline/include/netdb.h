/* Minimal POSIX resolver declarations for the offline WASI Preview1 build.
 * No DNS lookup or socket creation is delegated to the host. */
#ifndef PHP_WASM_OFFLINE_NETDB_H
#define PHP_WASM_OFFLINE_NETDB_H
#include <sys/socket.h>
#define AI_PASSIVE 1
#define AI_CANONNAME 2
#define AI_NUMERICHOST 4
#define AI_NUMERICSERV 1024
#define AI_ADDRCONFIG 32
#define NI_NUMERICHOST 1
#define NI_NUMERICSERV 2
#define NI_MAXHOST 1025
#define NI_MAXSERV 32
#define EAI_NONAME (-2)
#define EAI_FAIL (-4)
#define EAI_MEMORY (-10)
#define EAI_SYSTEM (-11)
struct addrinfo {
    int ai_flags, ai_family, ai_socktype, ai_protocol;
    socklen_t ai_addrlen;
    struct sockaddr *ai_addr;
    char *ai_canonname;
    struct addrinfo *ai_next;
};
struct hostent {
    char *h_name;
    char **h_aliases;
    int h_addrtype, h_length;
    char **h_addr_list;
};
int getaddrinfo(const char *, const char *, const struct addrinfo *, struct addrinfo **);
void freeaddrinfo(struct addrinfo *);
const char *gai_strerror(int);
struct hostent *gethostbyname(const char *);
#endif
