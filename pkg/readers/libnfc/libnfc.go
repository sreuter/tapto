package libnfc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/clausecker/nfc/v2"
	"github.com/rs/zerolog/log"
	"github.com/wizzomafizzo/tapto/pkg/config"
	"github.com/wizzomafizzo/tapto/pkg/readers"
	"github.com/wizzomafizzo/tapto/pkg/readers/libnfc/tags"
	"github.com/wizzomafizzo/tapto/pkg/tokens"
	"github.com/wizzomafizzo/tapto/pkg/utils"
)

const (
	timeToForgetCard   = 500 * time.Millisecond
	connectMaxTries    = 10
	timesToPoll        = 1
	periodBetweenPolls = 250 * time.Millisecond
	periodBetweenLoop  = 250 * time.Millisecond
)

const (
	ReaderTypePN532   = "PN532"
	ReaderTypeACR122U = "ACR122U"
	ReaderTypeUnknown = "Unknown"
)

type Reader struct {
	cfg          *config.UserConfig
	conn         string
	pnd          *nfc.Device
	writeRequest string
	writeError   error
	polling      bool
	prevToken    *tokens.Token
}

func NewReader(cfg *config.UserConfig) *Reader {
	return &Reader{
		cfg: cfg,
	}
}

func (r *Reader) Open(device string, iq chan<- readers.Scan) error {
	pnd, err := openDeviceWithRetries(device)
	if err != nil {
		return err
	}

	r.conn = device
	r.pnd = &pnd
	r.polling = true
	r.prevToken = nil

	go func() {
		for r.polling {
			if r.writeRequest != "" {
				r.writeTag()
				continue
			}

			token, removed, err := r.pollDevice(r.pnd, r.prevToken, timesToPoll, periodBetweenPolls)
			if errors.Is(err, nfc.Error(nfc.EIO)) {
				log.Error().Msgf("error during poll: %s", err)
				log.Error().Msg("fatal IO error, device was possibly unplugged")

				err = r.Close()
				if err != nil {
					log.Warn().Msgf("error closing device: %s", err)
				}

				continue
			} else if err != nil {
				log.Error().Msgf("error polling device: %s", err)
				continue
			}

			if removed {
				log.Info().Msg("token removed, sending to input queue")
				iq <- readers.Scan{
					Source: r.conn,
					Token:  nil,
				}
				r.prevToken = nil
			} else if token != nil {
				if r.prevToken != nil && token.UID == r.prevToken.UID {
					continue
				}

				log.Info().Msg("new token detected, sending to input queue")
				iq <- readers.Scan{
					Source: r.conn,
					Token:  token,
				}
				r.prevToken = token
			}

			time.Sleep(periodBetweenLoop)
		}
	}()

	return nil
}

func (r *Reader) Close() error {
	r.polling = false
	r.writeRequest = ""
	r.writeError = nil

	if r.pnd == nil {
		return nil
	} else {
		return r.pnd.Close()
	}
}

func (r *Reader) Ids() []string {
	return []string{
		"pn532_uart",
		"acr122_usb",
	}
}

func (r *Reader) Detect(connected []string) string {
	if !r.cfg.GetProbeDevice() {
		log.Debug().Msg("device probing disabled")
		return ""
	}

	device := detectSerialReaders(connected)
	if device == "" {
		// log.Debug().Msg("no serial nfc reader detected")
		return ""
	}

	if utils.Contains(connected, device) {
		log.Info().Msgf("already connected to: %s", device)
		return ""
	}

	log.Info().Msgf("detected nfc reader: %s", device)

	return device
}

func (r *Reader) Device() string {
	return r.conn
}

func (r *Reader) Connected() bool {
	return r.pnd != nil && r.pnd.Connection() != ""
}

func (r *Reader) Info() string {
	if !r.Connected() {
		return ""
	}

	// TODO: this conversation stuff is only necessary in the legacy socket
	// output. it should be moved to there and make Info output the full
	// libnfc device string

	connProto := strings.SplitN(strings.ToLower(r.conn), ":", 2)[0]
	deviceName := r.pnd.String()

	if connProto == "pn532_uart" {
		return ReaderTypePN532
	} else if strings.Contains(deviceName, "ACR122U") {
		return ReaderTypeACR122U
	} else {
		return ReaderTypeUnknown
	}
}

func (r *Reader) Write(text string) error {
	r.writeRequest = text

	for r.writeRequest != "" {
		time.Sleep(100 * time.Millisecond)
	}

	return r.writeError
}

func detectSerialReaders(connected []string) string {
	devices, err := utils.GetLinuxSerialDeviceList()
	if err != nil {
		log.Error().Msgf("error getting serial devices: %s", err)
		return ""
	}

	for _, device := range devices {
		connectionString := "pn532_uart:" + device

		if utils.Contains(connected, connectionString) {
			continue
		}

		pnd, err := nfc.Open(connectionString)
		if err == nil {
			pnd.Close()
			return connectionString
		}
	}

	return ""
}

func openDeviceWithRetries(device string) (nfc.Device, error) {
	tries := 0
	for {
		pnd, err := nfc.Open(device)
		if err == nil {
			log.Info().Msgf("successful connect, after %d tries", tries)

			connProto := strings.SplitN(strings.ToLower(device), ":", 2)[0]
			log.Info().Msgf("connection protocol: %s", connProto)
			deviceName := pnd.String()
			log.Info().Msgf("device name: %s", deviceName)

			if err := pnd.InitiatorInit(); err != nil {
				log.Error().Msgf("could not init initiator: %s", err)
				continue
			}

			return pnd, err
		}

		if tries >= connectMaxTries {
			// log.Debug().Msgf("could not open device after %d tries: %s", connectMaxTries, err)
			return pnd, err
		}

		tries++
	}
}

func (r *Reader) pollDevice(
	pnd *nfc.Device,
	activeToken *tokens.Token,
	ttp int,
	pbp time.Duration,
) (*tokens.Token, bool, error) {
	removed := false

	count, target, err := pnd.InitiatorPollTarget(tags.SupportedCardTypes, ttp, pbp)
	if err != nil && !errors.Is(err, nfc.Error(nfc.ETIMEOUT)) {
		return nil, removed, err
	}

	if count > 1 {
		log.Info().Msg("more than one card on the reader")
	}

	if count <= 0 {
		if activeToken != nil && time.Since(activeToken.ScanTime) > timeToForgetCard {
			log.Info().Msg("card removed")
			activeToken = nil
			removed = true
		}

		return activeToken, removed, nil
	}

	tagUid := tags.GetTagUID(target)
	if tagUid == "" {
		log.Warn().Msgf("unable to detect token UID: %s", target.String())
	}

	// no change in tag
	if activeToken != nil && tagUid == activeToken.UID {
		return activeToken, removed, nil
	}

	log.Info().Msgf("found token UID: %s", tagUid)

	var record tags.TagData
	cardType := tags.GetTagType(target)

	if cardType == tokens.TypeNTAG {
		log.Info().Msg("NTAG detected")
		record, err = tags.ReadNtag(*pnd)
		if err != nil {
			return activeToken, removed, fmt.Errorf("error reading ntag: %s", err)
		}
		cardType = tokens.TypeNTAG
	} else if cardType == tokens.TypeMifare {
		log.Info().Msg("MIFARE detected")
		record, err = tags.ReadMifare(*pnd, tagUid)
		if err != nil {
			log.Error().Msgf("error reading mifare: %s", err)
		}
		cardType = tokens.TypeMifare
	}

	log.Debug().Msgf("record bytes: %s", hex.EncodeToString(record.Bytes))
	tagText, err := tags.ParseRecordText(record.Bytes)
	if err != nil {
		log.Error().Err(err).Msgf("error parsing NDEF record")
		tagText = ""
	}

	if tagText == "" {
		log.Warn().Msg("no text NDEF found")
	} else {
		log.Info().Msgf("decoded text NDEF: %s", tagText)
	}

	card := &tokens.Token{
		Type:     record.Type,
		UID:      tagUid,
		Text:     tagText,
		Data:     hex.EncodeToString(record.Bytes),
		ScanTime: time.Now(),
	}

	return card, removed, nil
}

func (r *Reader) writeTag() {
	log.Info().Msgf("write request: %s", r.writeRequest)

	var count int
	var target nfc.Target
	var err error
	tries := 4 * 30 // ~30 seconds

	for tries > 0 {
		count, target, err = r.pnd.InitiatorPollTarget(
			tags.SupportedCardTypes,
			timesToPoll,
			periodBetweenPolls,
		)

		if err != nil && err.Error() != "timeout" {
			log.Error().Msgf("could not poll: %s", err)
		}

		if count > 0 {
			break
		}

		tries--
	}

	if count == 0 {
		log.Error().Msgf("could not detect a card")
		r.writeError = errors.New("could not detect a card")
		r.writeRequest = ""
		return
	}

	cardUid := tags.GetTagUID(target)
	log.Info().Msgf("found card with UID: %s", cardUid)

	cardType := tags.GetTagType(target)
	var bytesWritten []byte

	switch cardType {
	case tokens.TypeMifare:
		bytesWritten, err = tags.WriteMifare(*r.pnd, r.writeRequest, cardUid)
		if err != nil {
			log.Error().Msgf("error writing to mifare: %s", err)
			r.writeError = err
			r.writeRequest = ""
			return
		}
	case tokens.TypeNTAG:
		bytesWritten, err = tags.WriteNtag(*r.pnd, r.writeRequest)
		if err != nil {
			log.Error().Msgf("error writing to ntag: %s", err)
			r.writeError = err
			r.writeRequest = ""
			return
		}
	default:
		log.Error().Msgf("unsupported card type: %s", cardType)
		r.writeError = errors.New("unsupported card type")
		r.writeRequest = ""
		return
	}

	log.Info().Msgf("successfully wrote to card: %s", hex.EncodeToString(bytesWritten))
	r.writeError = nil
	r.writeRequest = ""
}
