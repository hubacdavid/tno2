package backend

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/conas/tno2/util/str"
)

type SimpleCodec struct {
}

func (sc *SimpleCodec) Decode(buf []byte) (int8, string, string, interface{}) {
	data := string(buf)
	nd := strings.Split(data, ":")
	msgTypeCode, _ := strconv.ParseInt(nd[0], 10, 8)
	conversationID := nd[1]
	msgName := nd[2]
	msgData := fromUrlQ(nd[3])

	switch int8(msgTypeCode) {
	case BE_ACTION_RS:
		return BE_ACTION_RS, conversationID, msgName, msgData
	case BE_GET_PROP_RS:
		return BE_GET_PROP_RS, conversationID, msgName, msgData
	case BE_EVENT:
		return BE_EVENT, "", msgName, msgData
	default:
		return BE_UNKNOWN_MSG_TYPE, "", "", nil
	}
}

func fromUrlQ(data string) map[string][]string {
	m, _ := url.ParseQuery(data)
	return m
}

func (sc *SimpleCodec) Encode(msgType int8, conversationID string, msgName string, data interface{}) []byte {
	d := data.(map[string]interface{})
	ds := str.Concat(msgType, ":", conversationID, ":", msgName, ":", toUrlQ(d))
	return []byte(ds)
}

func toUrlQ(data map[string]interface{}) string {
	log.Info("toUrlQ", data)

	params := url.Values{}
	for k, v := range data {
		params.Add(k, fmt.Sprintf("%v", v))
	}
	return params.Encode()
}
