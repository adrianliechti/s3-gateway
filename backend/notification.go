package backend

import "encoding/xml"

type NotificationConfiguration struct {
	XMLName xml.Name             `xml:"NotificationConfiguration" json:"-"`
	XMLNS   string               `xml:"xmlns,attr,omitempty" json:"-"`
	Topics  []TopicConfiguration `xml:"TopicConfiguration" json:",omitempty"`
	Unknown []UnknownElement     `xml:",any" json:"-"`
}
type TopicConfiguration struct {
	ID      string `xml:"Id,omitempty"`
	Topic   string
	Events  []string            `xml:"Event"`
	Filter  *NotificationFilter `xml:"Filter,omitempty" json:",omitempty"`
	Unknown []UnknownElement    `xml:",any" json:"-"`
}
type NotificationFilter struct {
	Key     *NotificationKeyFilter `xml:"S3Key"`
	Unknown []UnknownElement       `xml:",any" json:"-"`
}
type NotificationKeyFilter struct {
	Rules   []NotificationFilterRule `xml:"FilterRule"`
	Unknown []UnknownElement         `xml:",any" json:"-"`
}
type NotificationFilterRule struct {
	Name, Value string
	Unknown     []UnknownElement `xml:",any" json:"-"`
}
