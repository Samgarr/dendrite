package shared

import (
	"context"
	"encoding/json"
	"sync"

	eduAPI "github.com/matrix-org/dendrite/eduserver/api"
	"github.com/matrix-org/dendrite/syncapi/types"
	"github.com/matrix-org/gomatrixserverlib"
)

type ReceiptStreamProvider struct {
	DB          *Database
	latest      types.StreamPosition
	latestMutex sync.RWMutex
	update      *sync.Cond
}

func (p *ReceiptStreamProvider) StreamSetup() {
	locker := &sync.Mutex{}
	p.update = sync.NewCond(locker)

	latest, err := p.DB.Receipts.SelectMaxReceiptID(context.Background(), nil)
	if err != nil {
		return
	}

	p.latest = types.StreamPosition(latest)
}

func (p *ReceiptStreamProvider) StreamAdvance(
	latest types.StreamPosition,
) {
	p.latestMutex.Lock()
	defer p.latestMutex.Unlock()

	if latest > p.latest {
		p.latest = latest
		p.update.Broadcast()
	}
}

func (p *ReceiptStreamProvider) StreamRange(
	ctx context.Context,
	req *types.StreamRangeRequest,
	from, to types.StreamingToken,
) types.StreamingToken {
	var joinedRooms []string
	for roomID, membership := range req.Rooms {
		if membership == gomatrixserverlib.Join {
			joinedRooms = append(joinedRooms, roomID)
		}
	}

	lastPos, receipts, err := p.DB.Receipts.SelectRoomReceiptsAfter(context.TODO(), joinedRooms, from.ReceiptPosition)
	if err != nil {
		return types.StreamingToken{} //fmt.Errorf("unable to select receipts for rooms: %w", err)
	}

	// Group receipts by room, so we can create one ClientEvent for every room
	receiptsByRoom := make(map[string][]eduAPI.OutputReceiptEvent)
	for _, receipt := range receipts {
		receiptsByRoom[receipt.RoomID] = append(receiptsByRoom[receipt.RoomID], receipt)
	}

	for roomID, receipts := range receiptsByRoom {
		jr := req.Response.Rooms.Join[roomID]
		var ok bool

		ev := gomatrixserverlib.ClientEvent{
			Type:   gomatrixserverlib.MReceipt,
			RoomID: roomID,
		}
		content := make(map[string]eduAPI.ReceiptMRead)
		for _, receipt := range receipts {
			var read eduAPI.ReceiptMRead
			if read, ok = content[receipt.EventID]; !ok {
				read = eduAPI.ReceiptMRead{
					User: make(map[string]eduAPI.ReceiptTS),
				}
			}
			read.User[receipt.UserID] = eduAPI.ReceiptTS{TS: receipt.Timestamp}
			content[receipt.EventID] = read
		}
		ev.Content, err = json.Marshal(content)
		if err != nil {
			return types.StreamingToken{} // err
		}

		jr.Ephemeral.Events = append(jr.Ephemeral.Events, ev)
		req.Response.Rooms.Join[roomID] = jr
	}

	if lastPos > 0 {
		return types.StreamingToken{
			ReceiptPosition: lastPos,
		}
	} else {
		return types.StreamingToken{
			ReceiptPosition: to.ReceiptPosition,
		}
	}
}

func (p *ReceiptStreamProvider) StreamNotifyAfter(
	ctx context.Context,
	from types.StreamingToken,
) chan struct{} {
	ch := make(chan struct{})

	check := func() bool {
		p.latestMutex.RLock()
		defer p.latestMutex.RUnlock()
		if p.latest > from.ReceiptPosition {
			close(ch)
			return true
		}
		return false
	}

	// If we've already advanced past the specified position
	// then return straight away.
	if check() {
		return ch
	}

	// If we haven't, then we'll subscribe to updates. The
	// sync.Cond will fire every time the latest position
	// updates, so we can check and see if we've advanced
	// past it.
	go func(p *ReceiptStreamProvider) {
		p.update.L.Lock()
		defer p.update.L.Unlock()

		for {
			select {
			case <-ctx.Done():
				// The context has expired, so there's no point
				// in continuing to wait for the update.
				return
			default:
				// The latest position has been advanced. Let's
				// see if it's advanced to the position we care
				// about. If it has then we'll return.
				p.update.Wait()
				if check() {
					return
				}
			}
		}
	}(p)

	return ch
}

func (p *ReceiptStreamProvider) StreamLatestPosition(
	ctx context.Context,
) types.StreamingToken {
	p.latestMutex.RLock()
	defer p.latestMutex.RUnlock()

	return types.StreamingToken{
		ReceiptPosition: p.latest,
	}
}
