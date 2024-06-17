package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackpal/bencode-go"
)

const Port uint16 = 6881

type bencodeInfo struct {
	Pieces      string `bencode:"pieces"`
	PieceLength int    `bencode:"piece length"`
	Length      int    `bencode:"length"`
	Name        string `bencode:"name"`
}

type bencodeTorrent struct {
	Announce string      `bencode:"announce"`
	Info     bencodeInfo `bencode:"info"`
}

type TorrentInfo struct {
	Announce    string
	InfoHash    [20]byte
	PiecesHash  [][20]byte
	PieceLength int
	Length      int
	Name        string
}

type bencodeTrackerResponse struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

type Peer struct {
	IP   net.IP
	Port uint16
}

type Handshake struct {
	Pstr     string
	InfoHash [20]byte
	PeerID   [20]byte
}

func (h *Handshake) Serialize() []byte {
	buf := make([]byte, len(h.Pstr)+49)
	buf[0] = byte(len(h.Pstr))
	i := 1
	i += copy(buf[i:], h.Pstr)
	i += copy(buf[i:], make([]byte, 8))
	i += copy(buf[i:], h.InfoHash[:])
	i += copy(buf[i:], h.PeerID[:])
	return buf
}

// Read parses a handshake from a stream
func Read(r io.Reader) (*Handshake, error) {
	b := make([]byte, 1)
	_, err := io.ReadFull(r, b)
	if err != nil {
		return nil, err
	}

	pstrLen := int(b[0])
	if pstrLen == 0 {
		return nil, fmt.Errorf("Handshake recieved pstrLen should not be 0")
	}
	b = make([]byte, 48+pstrLen)
	io.ReadFull(r, b)
	var infoHash, peerId [20]byte
	copy(infoHash[:], b[pstrLen+8:pstrLen+8+20])
	copy(peerId[:], b[pstrLen+8+20:])
	h := Handshake{
		Pstr:     string(b[0:pstrLen]),
		InfoHash: infoHash,
		PeerID:   peerId,
	}
	return &h, nil
}

func UnmarshalPeerBin(peersBin []byte) ([]Peer, error) {
	numBytesPeer := 6
	if len(peersBin)%numBytesPeer != 0 {
		return nil, fmt.Errorf("recieved malformed peers binary")
	}
	numPeers := len(peersBin) / numBytesPeer
	peers := make([]Peer, numPeers)
	for i := 0; i < numPeers; i++ {
		offset := i * numBytesPeer

		peers[i].IP = net.IP(peersBin[offset : offset+4])
		peers[i].Port = binary.BigEndian.Uint16(peersBin[offset+4 : offset+6])
	}
	return peers, nil
}

func (btorInfo *bencodeInfo) Hash() ([20]byte, error) {
	var buf bytes.Buffer
	err := bencode.Marshal(&buf, *btorInfo)
	if err != nil {
		return [20]byte{}, err
	}
	return sha1.Sum(buf.Bytes()), nil
}

func (btorInfo *bencodeInfo) splitHashes() ([][20]byte, error) {
	pieceHashLen := 20 // no of bytes of SHA 1
	piecesBytes := []byte(btorInfo.Pieces)

	if len(piecesBytes)%pieceHashLen != 0 {
		return nil, fmt.Errorf("malformed pieces of length %d", len(piecesBytes))
	}
	numOfHashes := len(piecesBytes) / pieceHashLen
	result := make([][20]byte, numOfHashes)
	for i := 0; i < numOfHashes; i++ {
		copy(result[i][:], piecesBytes[i*pieceHashLen:(i+1)*pieceHashLen])
	}
	return result, nil
}

func (btor *bencodeTorrent) toTorrentInfo() (*TorrentInfo, error) {
	var err error
	ti := TorrentInfo{}
	ti.Announce = btor.Announce
	ti.InfoHash, err = btor.Info.Hash()
	if err != nil {
		return nil, err
	}
	ti.Name = btor.Info.Name
	ti.Length = btor.Info.Length

	ti.PiecesHash, err = btor.Info.splitHashes()
	ti.PieceLength = btor.Info.PieceLength
	if err != nil {
		return nil, err
	}
	return &ti, nil
}

func Open(r io.Reader) (*bencodeTorrent, error) {
	btor := bencodeTorrent{}
	err := bencode.Unmarshal(r, &btor)
	if err != nil {
		return nil, err
	}
	return &btor, nil
}

func (t *TorrentInfo) buildTrackerURL(peerId [20]byte, port uint16) (string, error) {
	baseURL, err := url.Parse(t.Announce)
	if err != nil {
		return "", err
	}
	params := url.Values{
		"info_hash":  []string{string(t.InfoHash[:])},
		"peer_id":    []string{string(peerId[:])},
		"port":       []string{strconv.Itoa(int(port))},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(t.Length)},
	}

	baseURL.RawQuery = params.Encode()
	return baseURL.String(), nil
}

func (t *TorrentInfo) getPeers(peerId [20]byte, port uint16) ([]Peer, error) {
	urlString, err := t.buildTrackerURL(peerId, port)
	if err != nil {
		return nil, err
	}

	c := http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(urlString)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	trackerResponse := bencodeTrackerResponse{}
	err = bencode.Unmarshal(resp.Body, &trackerResponse)
	if err != nil {
		return nil, err
	}
	return UnmarshalPeerBin([]byte(trackerResponse.Peers))

}

func (p Peer) String() string {
	return net.JoinHostPort(p.IP.String(), strconv.Itoa(int(p.Port)))
}

func (p *Peer) makeConn() (net.Conn, error) {
	// start a tcp connection
	conn, err := net.Dial("tcp", p.String())
	if err != nil {
		fmt.Printf("err in making con %s\n", err)
		return nil, err
	}
	fmt.Printf("Connected to peer %+v\n", conn)
	return conn, nil
}

func main() {
	torrentFileName := "/home/kevin/debian.torrent"
	torrentFile, err := os.Open(torrentFileName)
	if err != nil {
		fmt.Printf("Error opening file: %s, error: %s", torrentFileName, err)
		return
	}
	btor, err := Open(torrentFile)
	if err != nil {
		fmt.Printf("Error unmarshalling torrent file: %s, error: %s", torrentFileName, err)
		return
	}

	// fmt.Printf("torrent info is: %+v", btor)
	torrentInfo, err := btor.toTorrentInfo()
	if err != nil {
		fmt.Printf("Error in torrent info conversion: %s", err)
	}
	// fmt.Printf("Torrent info: %+v", torrentInfo)

	var peerId [20]byte
	_, err = rand.Read(peerId[:])
	if err != nil {
		fmt.Printf("random produced error: %s", err)
	}

	peers, _ := torrentInfo.getPeers(peerId, Port)
	for i := 0; i < len(peers); i++ {
		go func(i int) {
			peer := peers[i]
			conn, err := peer.makeConn()
			if err != nil {
				fmt.Printf("peer connection failed for %d with err %s\n", i, err)
				return
			}
			h := Handshake{
				Pstr:     "BitTorrent protocol",
				InfoHash: torrentInfo.InfoHash,
				PeerID:   peerId,
			}
			fmt.Printf("Going to send handshake: %+v\n", h)
			handshakeBytes := h.Serialize()
			_, err = conn.Write(handshakeBytes)
			if err != nil {
				fmt.Printf("send handshake failed for %d with err %s\n", i, err)
				return
			}
			recievedHandshake, err := Read(conn)
			if err != nil {
				fmt.Printf("peer handshake failed for %d with err %s\n", i, err)
				return
			}
			fmt.Printf("rec handshake for %d is %+v", i, recievedHandshake)
		}(i)

		// conn.SetDeadline(time.Now().Add(5 * time.Second))

		// conn.SetDeadline(time.Time{})
	}
	fmt.Printf("peers: %+v\n", peers)
	time.Sleep(5 * time.Minute)

}
