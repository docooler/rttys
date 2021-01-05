package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"github.com/zhaojh329/rttys/cache"
)

type webNewCon struct {
	r *http.Request // First request
	b *bufio.Reader
	c net.Conn
}

type webCon struct {
	dev *device
	c   net.Conn
}

type webReq struct {
	id   uint16
	data []byte
	dev  *device
}

type webResp struct {
	data []byte
	dev  *device
}

var webCons = make(map[string]map[uint16]*webCon)
var webSessions *cache.Cache

func handleWebReq(req *webReq) {
	dev := req.dev

	if len(req.data) == 2 {
		delete(webCons[dev.id], req.id)
	}

	dev.writeMsg(msgTypeWeb, req.data)
}

func handleWebResp(resp *webResp) {
	data := resp.data
	id := binary.BigEndian.Uint16(data[:2])
	data = data[2:]

	devcons, ok := webCons[resp.dev.id]
	if !ok {
		return
	}

	wc, ok := devcons[id]
	if !ok {
		return
	}

	c := wc.c

	if len(data) == 0 {
		delete(devcons, id)
		c.Close()
		return
	}

	c.Write(data)
}

func makeWebReqMsg(br *broker, dev *device, id uint16, c net.Conn, r *http.Request, hostHeaderRewrite string, destAddr []byte) {
	req := make([]byte, 2)
	binary.BigEndian.PutUint16(req, id)

	req = append(req, destAddr...)
	req = append(req, r.Method...)
	req = append(req, ' ')
	req = append(req, r.RequestURI...)
	req = append(req, ' ')
	req = append(req, "HTTP/1.1\r\n"...)

	for k, v := range r.Header {
		req = append(req, k...)
		req = append(req, ':')
		req = append(req, strings.Join(v, ",")...)
		req = append(req, "\r\n"...)
	}

	req = append(req, "Host:"...)
	req = append(req, hostHeaderRewrite...)
	req = append(req, "\r\n"...)

	req = append(req, "\r\n"...)

	b := make([]byte, 4096)
	n, _ := r.Body.Read(b)
	if n > 0 {
		req = append(req, b[:n]...)
	}

	br.webReq <- &webReq{id, req, dev}

	for {
		n, err := r.Body.Read(b)
		if n > 0 {
			req := make([]byte, 2)
			binary.BigEndian.PutUint16(req, id)

			req = append(req, destAddr...)
			req = append(req, b[:n]...)
			br.webReq <- &webReq{id, req, dev}
		}

		if err != nil {
			return
		}
	}
}

func genDestAddr(addr string) []byte {
	destIp, destPort, err := webReqVaildAddr(addr)
	if err != nil {
		return nil
	}

	b := make([]byte, 6)
	copy(b, destIp)

	binary.BigEndian.PutUint16(b[4:], destPort)

	return b
}

func handleWebCon(br *broker, wc *webNewCon) {
	c := wc.c
	r := wc.r

	cookie, err := r.Cookie("rtty-web-sid")
	if err != nil {
		c.Close()
		return
	}
	sid := cookie.Value

	var done chan struct{}
	if v, ok := webSessions.Get(sid); ok {
		webSessions.Active(sid, 0)
		done = v.(chan struct{})
	} else {
		c.Close()
		return
	}

	cookie, err = r.Cookie("rtty-web-devid")
	if err != nil {
		c.Close()
		return
	}
	devid := cookie.Value

	dev, ok := br.devices[devid]
	if !ok {
		c.Close()
		return
	}

	hostHeaderRewrite := "localhost"
	cookie, err = r.Cookie("rtty-web-destaddr")
	if err == nil {
		hostHeaderRewrite, _ = url.QueryUnescape(cookie.Value)
	}

	destAddr := genDestAddr(hostHeaderRewrite)

	if _, ok := webCons[devid]; !ok {
		webCons[devid] = make(map[uint16]*webCon)
	}

	var id uint16
	for ; id < 0xffff; id++ {
		if _, ok := webCons[devid][id]; !ok {
			break
		}
	}

	if id == 0xffff {
		log.Error().Msg("busy")
		c.Close()
		return
	}

	webCons[devid][id] = &webCon{dev, c}

	readEnd := make(chan struct{})

	go func() {
		defer func() {
			c.Close()
			req := make([]byte, 2)
			binary.BigEndian.PutUint16(req, id)
			br.webReq <- &webReq{id, req, dev}
			close(readEnd)
		}()

		makeWebReqMsg(br, dev, id, c, r, hostHeaderRewrite, destAddr)

		for {
			r, err := http.ReadRequest(wc.b)
			if err != nil {
				return
			}
			makeWebReqMsg(br, dev, id, c, r, hostHeaderRewrite, destAddr)
		}
	}()

	go func() {
		select {
		case <-done:
			c.Close()
		case <-readEnd:
		}
	}()
}

func listenDeviceWeb(br *broker) error {
	cfg := br.cfg

	addr, err := net.ResolveTCPAddr("tcp", cfg.addrWeb)
	if err != nil {
		return err
	}
	cfg.webPort = addr.Port

	webSessions = cache.New(30*time.Minute, 5*time.Second)

	log.Info().Msgf("Listen dev web on: %s", cfg.addrWeb)

	ln, err := net.Listen("tcp", cfg.addrWeb)
	if err != nil {
		return err
	}
	defer ln.Close()

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Error().Msg(err.Error())
			continue
		}

		b := bufio.NewReader(c)

		r, err := http.ReadRequest(b)
		if err != nil {
			continue
		}

		br.webCon <- &webNewCon{r, b, c}
	}
}

func webReqVaildAddr(addr string) (net.IP, uint16, error) {
	ips, ports, err := net.SplitHostPort(addr)
	if err != nil {
		ips = addr
		ports = "80"
	}

	ip := net.ParseIP(ips)
	if ip == nil {
		return nil, 0, errors.New("invalid IPv4 Addr")
	}

	ip = ip.To4()
	if ip == nil {
		return nil, 0, errors.New("invalid IPv4 Addr")
	}

	port, _ := strconv.Atoi(ports)

	return ip, uint16(port), nil
}

func webReqRedirect(br *broker, cfg *rttysConfig, c *gin.Context) {
	devid := c.Param("devid")
	addr := c.Param("addr")
	path := c.Param("path")

	_, _, err := webReqVaildAddr(addr)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	_, ok := br.devices[devid]
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}

	host, _, err := net.SplitHostPort(c.Request.Host)
	if err != nil {
		host = c.Request.Host
	}

	location := "http://" + host

	if cfg.webPort != 80 {
		location += fmt.Sprintf(":%d", cfg.webPort)
	}

	location += path

	location += fmt.Sprintf("?_=%d", time.Now().Unix())

	sid, err := c.Cookie("rtty-web-sid")
	if err == nil {
		if v, ok := webSessions.Get(sid); ok {
			ch := v.(chan struct{})
			webSessions.Del(sid)
			close(ch)
		}
	}

	sid = genUniqueID("web")

	webSessions.Set(sid, make(chan struct{}), 0)

	c.SetCookie("rtty-web-sid", sid, 0, "", "", false, true)
	c.SetCookie("rtty-web-devid", devid, 0, "", "", false, true)
	c.SetCookie("rtty-web-destaddr", addr, 0, "", "", false, true)
	c.Redirect(http.StatusFound, location)
}