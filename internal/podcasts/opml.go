package podcasts

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// opmlDocument modela la estructura completa de un archivo OPML 2.0.
type opmlDocument struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title       string `xml:"title"`
	DateCreated string `xml:"dateCreated,omitempty"`
}

type opmlBody struct {
	Outlines []opmlNode `xml:"outline"`
}

type opmlNode struct {
	Text     string     `xml:"text,attr"`
	Title    string     `xml:"title,attr"`
	Type     string     `xml:"type,attr"`
	XMLURL   string     `xml:"xmlUrl,attr"`
	URL      string     `xml:"url,attr"`
	HTMLURL  string     `xml:"htmlUrl,attr"`
	Outlines []opmlNode `xml:"outline"`
}

// collectOutlines recorre de forma recursiva todos los niveles de nodos outline extrayendo feeds RSS.
func collectOutlines(nodes []opmlNode, list *[]OPMLOutline) {
	for _, n := range nodes {
		xmlURL := strings.TrimSpace(n.XMLURL)
		if xmlURL == "" {
			xmlURL = strings.TrimSpace(n.URL)
		}

		if xmlURL != "" {
			title := strings.TrimSpace(n.Title)
			if title == "" {
				title = strings.TrimSpace(n.Text)
			}
			text := strings.TrimSpace(n.Text)
			if text == "" {
				text = title
			}

			*list = append(*list, OPMLOutline{
				Title:   title,
				Text:    text,
				Type:    strings.TrimSpace(n.Type),
				XMLURL:  xmlURL,
				HTMLURL: strings.TrimSpace(n.HTMLURL),
			})
		}

		if len(n.Outlines) > 0 {
			collectOutlines(n.Outlines, list)
		}
	}
}

// ParseOPML interpreta los bytes de un archivo OPML y extrae la lista de feeds de podcast descubiertos.
func ParseOPML(data []byte) ([]OPMLOutline, error) {
	var doc opmlDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("formato OPML o XML no válido: %w", err)
	}

	var results []OPMLOutline
	collectOutlines(doc.Body.Outlines, &results)
	return results, nil
}

// GenerateOPML genera un documento OPML 2.0 estándar en bytes con todas las suscripciones de podcast actuales.
func GenerateOPML(podcasts []Podcast) ([]byte, error) {
	doc := opmlDocument{
		Version: "2.0",
		Head: opmlHead{
			Title:       "Suscripciones de Podcasts de AuraBox",
			DateCreated: time.Now().Format(time.RFC1123Z),
		},
		Body: opmlBody{
			Outlines: make([]opmlNode, 0, len(podcasts)),
		},
	}

	for _, p := range podcasts {
		doc.Body.Outlines = append(doc.Body.Outlines, opmlNode{
			Text:    p.Title,
			Title:   p.Title,
			Type:    "rss",
			XMLURL:  p.FeedURL,
			HTMLURL: p.Link,
		})
	}

	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("error al codificar OPML: %w", err)
	}

	return buf.Bytes(), nil
}
