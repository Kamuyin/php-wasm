<?php
/**
 * WASI compatibility shim (auto-loaded must-use plugin).
 *
 * WASI Preview1 has no network sockets. WordPress's installer calls
 * wp_install_maybe_enable_pretty_permalinks(), which issues a loopback
 * HTTP request that would reach Fsockopen and crash with a fatal
 * "undefined function stream_socket_client()" error.
 *
 * This filter runs inside WP_Http::request() *before* a transport is
 * selected, so it prevents the crash entirely. It returns a fake 200
 * for loopback calls (so WP enables pretty permalinks) and a WP_Error
 * for all other outbound requests (so WP degrades gracefully instead of
 * hanging or crashing).
 */
add_filter( 'pre_http_request', static function ( $preempt, $args, $url ) {
    if ( false !== $preempt ) {
        return $preempt; // already preempted upstream
    }

    // Loopback requests (installer permalink test, health checks, etc.)
    // Return a fake 200 so WordPress believes URL rewriting works.
    if ( preg_match( '#^https?://(localhost|127\.0\.0\.1)(:\d+)?(/|$)#', $url ) ) {
        return [
            'headers'       => [],
            'body'          => '',
            'response'      => [ 'code' => 200, 'message' => 'OK' ],
            'cookies'       => [],
            'http_response' => null,
        ];
    }

    // All other outbound requests: return a clear error rather than a fatal crash.
    return new WP_Error(
        'http_request_failed',
        'WASI Preview1: no outbound network sockets. Outbound HTTP is not available.'
    );
}, 10, 3 );
