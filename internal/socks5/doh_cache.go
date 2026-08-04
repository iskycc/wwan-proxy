package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

type dnsCacheEntry struct {
	response []byte
	storedAt time.Time
	expires  time.Time
}

type dnsFlight struct {
	done     chan struct{}
	response []byte
	err      error
}

type dnsRecord struct {
	recordType  uint16
	recordClass uint16
	ttlOffset   int
	ttl         uint32
	dataStart   int
	dataEnd     int
}

type dnsLayout struct {
	questions  []dnsQuestion
	answers    []dnsRecord
	authority  []dnsRecord
	additional []dnsRecord
}

type dnsQuestion struct {
	name        string
	recordType  uint16
	recordClass uint16
}

func (d *dohResolver) cachedQuery(ctx context.Context, query []byte, resolve func(context.Context, []byte) ([]byte, error)) ([]byte, error) {
	key, _, keyErr := dnsCacheKey(query)
	if keyErr != nil {
		return resolve(ctx, query)
	}
	now := time.Now()
	d.cacheMu.Lock()
	if entry, ok := d.cache[key]; ok {
		if now.Before(entry.expires) {
			response := ageDNSResponse(entry.response, query, now.Sub(entry.storedAt))
			d.cacheMu.Unlock()
			return response, nil
		}
		delete(d.cache, key)
	}
	if flight, ok := d.inflight[key]; ok {
		d.cacheMu.Unlock()
		select {
		case <-flight.done:
			if flight.err != nil {
				return nil, flight.err
			}
			return withDNSQuery(flight.response, query), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &dnsFlight{done: make(chan struct{})}
	d.inflight[key] = flight
	d.cacheMu.Unlock()

	response, err := resolve(ctx, query)
	now = time.Now()
	d.cacheMu.Lock()
	if err == nil {
		flight.response = append([]byte(nil), response...)
		if ttl, ttlErr := dnsResponseCacheTTL(response); ttlErr == nil && ttl > 0 {
			if len(d.cache) > 0 && len(d.cache)%256 == 0 {
				for cachedKey, entry := range d.cache {
					if !now.Before(entry.expires) {
						delete(d.cache, cachedKey)
					}
				}
			}
			d.cache[key] = dnsCacheEntry{
				response: append([]byte(nil), response...),
				storedAt: now,
				expires:  now.Add(time.Duration(ttl) * time.Second),
			}
		}
	} else {
		flight.err = err
	}
	delete(d.inflight, key)
	close(flight.done)
	d.cacheMu.Unlock()
	return response, err
}

func dnsCacheKey(wire []byte) (string, uint16, error) {
	layout, err := parseDNSLayout(wire)
	if err != nil || len(layout.questions) != 1 {
		return "", 0, errors.New("DNS cache requires exactly one valid question")
	}
	question := layout.questions[0]
	flags := binary.BigEndian.Uint16(wire[2:4]) & 0x7910 // opcode, Recursion Desired and Checking Disabled
	do := false
	var optClass uint16
	var optTTL uint32
	var optData []byte
	for _, record := range layout.additional {
		if record.recordType == 41 {
			do = record.ttl&0x8000 != 0
			optClass = record.recordClass
			optTTL = record.ttl
			optData = wire[record.dataStart:record.dataEnd]
			break
		}
	}
	key := fmt.Sprintf("%s|%d|%d|%04x|%t|%d|%08x|%x", strings.ToLower(question.name), question.recordType, question.recordClass, flags, do, optClass, optTTL, optData)
	return key, binary.BigEndian.Uint16(wire[:2]), nil
}

func validateDNSResponse(query, response []byte) error {
	if len(query) < 12 || len(response) < 12 {
		return errors.New("DNS message is shorter than its header")
	}
	queryFlags := binary.BigEndian.Uint16(query[2:4])
	responseFlags := binary.BigEndian.Uint16(response[2:4])
	if responseFlags&0x8000 == 0 {
		return errors.New("DoH response does not have the DNS response flag")
	}
	if responseFlags&0x0200 != 0 {
		return errors.New("DoH response is truncated")
	}
	if queryFlags&0x7800 != responseFlags&0x7800 {
		return errors.New("DoH response opcode does not match its query")
	}
	if binary.BigEndian.Uint16(query[:2]) != binary.BigEndian.Uint16(response[:2]) {
		return errors.New("DoH response ID does not match its query")
	}
	queryLayout, err := parseDNSLayout(query)
	if err != nil {
		return fmt.Errorf("parse DNS query: %w", err)
	}
	responseLayout, err := parseDNSLayout(response)
	if err != nil {
		return fmt.Errorf("parse DoH DNS response: %w", err)
	}
	rcode := uint16(responseFlags & 0x000f)
	for _, record := range responseLayout.additional {
		if record.recordType == 41 {
			rcode |= uint16(record.ttl>>24) << 4
			break
		}
	}
	if rcode != 0 && rcode != 3 {
		return fmt.Errorf("DoH response has retryable DNS response code %d", rcode)
	}
	if len(queryLayout.questions) != len(responseLayout.questions) {
		return errors.New("DoH response question count does not match its query")
	}
	for i := range queryLayout.questions {
		queryQuestion := queryLayout.questions[i]
		responseQuestion := responseLayout.questions[i]
		if !strings.EqualFold(queryQuestion.name, responseQuestion.name) || queryQuestion.recordType != responseQuestion.recordType || queryQuestion.recordClass != responseQuestion.recordClass {
			return errors.New("DoH response question does not match its query")
		}
	}
	return nil
}

func dnsResponseCacheTTL(wire []byte) (uint32, error) {
	layout, err := parseDNSLayout(wire)
	if err != nil {
		return 0, err
	}
	if len(layout.answers) > 0 {
		return minimumRecordTTL(layout.answers), nil
	}
	// RFC 2308 negative caching uses the smaller of the SOA RR TTL and the
	// SOA.MINIMUM field for both NXDOMAIN and NODATA responses.
	var ttl uint32
	found := false
	for _, record := range layout.authority {
		if record.recordType != 6 {
			continue
		}
		minimum, err := soaMinimum(wire, record)
		if err != nil {
			return 0, err
		}
		candidate := min(record.ttl, minimum)
		if !found || candidate < ttl {
			ttl = candidate
			found = true
		}
	}
	return ttl, nil
}

func minimumRecordTTL(records []dnsRecord) uint32 {
	var ttl uint32
	found := false
	for _, record := range records {
		if record.recordType == 41 {
			continue
		}
		if !found || record.ttl < ttl {
			ttl = record.ttl
			found = true
		}
	}
	return ttl
}

func soaMinimum(wire []byte, record dnsRecord) (uint32, error) {
	offset, err := skipDNSName(wire, record.dataStart)
	if err != nil || offset > record.dataEnd {
		return 0, errors.New("invalid SOA MNAME")
	}
	offset, err = skipDNSName(wire, offset)
	if err != nil || offset+20 > record.dataEnd {
		return 0, errors.New("invalid SOA RNAME or timing fields")
	}
	return binary.BigEndian.Uint32(wire[offset+16 : offset+20]), nil
}

func ageDNSResponse(response, query []byte, elapsed time.Duration) []byte {
	aged := withDNSQuery(response, query)
	layout, err := parseDNSLayout(aged)
	if err != nil {
		return aged
	}
	seconds := uint64(elapsed / time.Second)
	for _, section := range [][]dnsRecord{layout.answers, layout.authority, layout.additional} {
		for _, record := range section {
			if record.recordType == 41 {
				continue
			}
			remaining := uint32(0)
			if uint64(record.ttl) > seconds {
				remaining = record.ttl - uint32(seconds)
			}
			binary.BigEndian.PutUint32(aged[record.ttlOffset:record.ttlOffset+4], remaining)
		}
	}
	return aged
}

func withDNSQuery(wire, query []byte) []byte {
	result := append([]byte(nil), wire...)
	if len(result) < 12 || len(query) < 12 {
		return result
	}
	binary.BigEndian.PutUint16(result[:2], binary.BigEndian.Uint16(query[:2]))
	// Go's resolver may randomize the case of query labels as a spoofing
	// defense. Preserve that per-request question on cache and singleflight
	// hits while leaving answer compression pointers at the same offsets.
	responseQuestionEnd, responseErr := skipDNSName(result, 12)
	queryQuestionEnd, queryErr := skipDNSName(query, 12)
	if responseErr == nil && queryErr == nil && responseQuestionEnd == queryQuestionEnd && queryQuestionEnd+4 <= len(query) && responseQuestionEnd+4 <= len(result) {
		copy(result[12:responseQuestionEnd+4], query[12:queryQuestionEnd+4])
	}
	return result
}

func parseDNSLayout(wire []byte) (dnsLayout, error) {
	var layout dnsLayout
	if len(wire) < 12 {
		return layout, errors.New("DNS message is shorter than its header")
	}
	offset := 12
	questionCount := int(binary.BigEndian.Uint16(wire[4:6]))
	for range questionCount {
		name, next, err := decodeDNSName(wire, offset)
		if err != nil || next+4 > len(wire) {
			return layout, errors.New("invalid DNS question")
		}
		layout.questions = append(layout.questions, dnsQuestion{
			name:        name,
			recordType:  binary.BigEndian.Uint16(wire[next : next+2]),
			recordClass: binary.BigEndian.Uint16(wire[next+2 : next+4]),
		})
		offset = next + 4
	}
	counts := []int{
		int(binary.BigEndian.Uint16(wire[6:8])),
		int(binary.BigEndian.Uint16(wire[8:10])),
		int(binary.BigEndian.Uint16(wire[10:12])),
	}
	sections := []*[]dnsRecord{&layout.answers, &layout.authority, &layout.additional}
	for i, count := range counts {
		for range count {
			next, err := skipDNSName(wire, offset)
			if err != nil || next+10 > len(wire) {
				return layout, errors.New("invalid DNS resource record")
			}
			dataLength := int(binary.BigEndian.Uint16(wire[next+8 : next+10]))
			dataStart := next + 10
			if dataStart+dataLength > len(wire) {
				return layout, errors.New("invalid DNS resource data length")
			}
			*sections[i] = append(*sections[i], dnsRecord{
				recordType:  binary.BigEndian.Uint16(wire[next : next+2]),
				recordClass: binary.BigEndian.Uint16(wire[next+2 : next+4]),
				ttlOffset:   next + 4,
				ttl:         binary.BigEndian.Uint32(wire[next+4 : next+8]),
				dataStart:   dataStart,
				dataEnd:     dataStart + dataLength,
			})
			offset = dataStart + dataLength
		}
	}
	if offset != len(wire) {
		return layout, errors.New("DNS message contains trailing data")
	}
	return layout, nil
}

func decodeDNSName(wire []byte, start int) (string, int, error) {
	var labels []string
	offset := start
	next := -1
	visited := make(map[int]struct{})
	for steps := 0; steps < 128; steps++ {
		if offset >= len(wire) {
			return "", 0, errors.New("DNS name exceeds message")
		}
		if _, exists := visited[offset]; exists {
			return "", 0, errors.New("DNS compression pointer loop")
		}
		visited[offset] = struct{}{}
		length := int(wire[offset])
		if length&0xc0 == 0xc0 {
			if offset+2 > len(wire) {
				return "", 0, errors.New("short DNS compression pointer")
			}
			if next < 0 {
				next = offset + 2
			}
			offset = int(binary.BigEndian.Uint16(wire[offset:offset+2]) & 0x3fff)
			continue
		}
		if length&0xc0 != 0 || length > 63 {
			return "", 0, errors.New("invalid DNS label length")
		}
		offset++
		if length == 0 {
			if next < 0 {
				next = offset
			}
			return strings.Join(labels, "."), next, nil
		}
		if offset+length > len(wire) {
			return "", 0, errors.New("DNS label exceeds message")
		}
		labels = append(labels, string(wire[offset:offset+length]))
		offset += length
	}
	return "", 0, errors.New("DNS name has too many labels")
}
