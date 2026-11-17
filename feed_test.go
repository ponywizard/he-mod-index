package main

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestFeedEscapesContentAndTracksReleaseRevision(t *testing.T) {
	a := testApp(t)
	c := client(a, "192.0.2.1")
	c.register(t, "feed_author")
	m := c.addMod(t, "A <mod> & friends")
	m.ShortDescription = "<b>Plain text</b> & symbols"
	status(t, c.request("PUT", modPath(m), m.modInput, nil), 200)
	read := func() rssItem {
		t.Helper()
		w := client(a, "192.0.2.2").request("GET", "/feed.xml", nil, nil)
		status(t, w, 200)
		var f rssFeed
		if err := xml.Unmarshal(w.Body.Bytes(), &f); err != nil {
			t.Fatal(err)
		}
		if f.Version != "2.0" || len(f.Channel.Items) != 1 || !strings.Contains(w.Header().Get("Content-Type"), "application/rss+xml") {
			t.Fatal(w.Body.String())
		}
		return f.Channel.Items[0]
	}
	first := read()
	if first.Title != m.Name+" — "+m.Version || first.GUID.Permalink || first.Description != "&lt;b&gt;Plain text&lt;/b&gt; &amp; symbols" {
		t.Fatal(first)
	}
	m.Description = "Description change"
	status(t, c.request("PUT", modPath(m), m.modInput, nil), 200)
	if read().GUID != first.GUID {
		t.Fatal("description changed release GUID")
	}
	m.Version = "2.0"
	status(t, c.request("PUT", modPath(m), m.modInput, nil), 200)
	if read().GUID == first.GUID {
		t.Fatal("new release retained GUID")
	}
}
