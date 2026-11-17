package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"time"
)

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}
type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}
type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description string  `xml:"description"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
}
type rssGUID struct {
	Permalink bool   `xml:"isPermaLink,attr"`
	Value     string `xml:",chardata"`
}

func (a *app) feed(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.db.QueryContext(r.Context(), modSelect+" ORDER BY m.updated_at DESC,m.id DESC LIMIT 100", 0)
	if err != nil {
		return err
	}
	defer rows.Close()
	f := rssFeed{Version: "2.0", Channel: rssChannel{Title: "Home: Equestria mods", Link: a.cfg.PublicURL + "/", Description: "Mod releases and updates."}}
	for rows.Next() {
		m, err := scanMod(rows)
		if err != nil {
			return err
		}
		link := fmt.Sprintf("%s/mod/%d", a.cfg.PublicURL, m.ID)
		f.Channel.Items = append(f.Channel.Items, rssItem{Title: m.Name + " — " + m.Version, Link: link, Description: html.EscapeString(m.ShortDescription), GUID: rssGUID{Value: fmt.Sprintf("%s#revision-%d", link, m.Revision)}, PubDate: time.Unix(m.UpdatedAt, 0).UTC().Format(time.RFC1123Z)})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	data, err := xml.Marshal(f)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	_, err = w.Write(data)
	return err
}
