package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// Include special-use ranges that IsGlobalUnicast/IsPrivate do not exclude.
var blockedImageNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}
var imageIPv6Global = netip.MustParsePrefix("2000::/3")

func publicImageIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	// Restrict IPv6 to ordinary global addresses; excludes mapped/NAT64 local routes.
	if ip.Is6() && !imageIPv6Global.Contains(ip) {
		return false
	}
	for _, p := range blockedImageNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func imageURL(raw string) (*url.URL, error) {
	if !validURL(raw) {
		return nil, problem(400, "Use a public http(s) image URL.")
	}
	u, _ := url.Parse(raw)
	if p := u.Port(); p != "" && ((u.Scheme == "http" && p != "80") || (u.Scheme == "https" && p != "443")) {
		return nil, problem(400, "Image URLs must use the standard HTTP or HTTPS port.")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicImageIP(ip) {
		return nil, problem(400, "Image URL must address a public server.")
	}
	return u, nil
}

type imageResolver func(context.Context, string) ([]netip.Addr, error)
type imageDialer func(context.Context, string, string) (net.Conn, error)

func imageTransport(resolve imageResolver, dial imageDialer) *http.Transport {
	return &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			var ips []netip.Addr
			if ip, err := netip.ParseAddr(host); err == nil {
				ips = []netip.Addr{ip}
			} else {
				ips, err = resolve(ctx, host)
				if err != nil {
					return nil, err
				}
			}
			if len(ips) == 0 {
				return nil, errors.New("image hostname has no addresses")
			}
			for _, ip := range ips {
				if !publicImageIP(ip) {
					return nil, errors.New("image hostname includes a non-public address")
				}
			}
			var last error
			for _, ip := range ips {
				conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
	}
}
func newImageClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Timeout: 12 * time.Second, Transport: imageTransport(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}, dialer.DialContext),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many image redirects")
			}
			_, err := imageURL(req.URL.String())
			return err
		},
	}
}
func fetchImage(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	u, err := imageURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, problem(400, "Invalid image URL.")
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif")
	req.Header.Set("User-Agent", "he-mod-index/1.0")
	res, err := client.Do(req)
	if err != nil {
		return nil, problem(400, "Could not fetch a public image from this URL.")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, problem(400, "Image server did not return a successful response.")
	}
	if res.ContentLength > maxImageBytes {
		return nil, problem(413, "Image must be at most 4 MiB.")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxImageBytes+1))
	if err != nil {
		return nil, problem(400, "Could not finish downloading image.")
	}
	if len(data) > maxImageBytes {
		return nil, problem(413, "Image must be at most 4 MiB.")
	}
	return data, nil
}
