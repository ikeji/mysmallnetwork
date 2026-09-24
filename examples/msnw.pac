// Proxy auto-config for msnw: send msnw names to "msnw client --socks5",
// everything else directly. Use it as a "file://" or "http://" URL in the
// browser's automatic proxy configuration, or via the OS proxy settings.
//
// With the PAC, other sites keep working even while the proxy is not running.
// (Without a PAC, "msnw client --socks5" also reaches other sites directly.)

var MSNW_PROXY = "SOCKS5 127.0.0.1:1080; SOCKS 127.0.0.1:1080";

// Exporter names you use without the ".msnw" suffix (e.g. http://home/).
// Names ending in ".msnw" never need to be listed.
var MSNW_NAMES = ["home", "mypc"];

function FindProxyForURL(url, host) {
    host = host.toLowerCase();
    if (dnsDomainIs(host, ".msnw")) {
        return MSNW_PROXY;
    }
    for (var i = 0; i < MSNW_NAMES.length; i++) {
        if (host == MSNW_NAMES[i]) {
            return MSNW_PROXY;
        }
    }
    return "DIRECT";
}
