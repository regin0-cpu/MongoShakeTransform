package sourceReader

// read change stream event from source mongodb

import (
	"sync"
	"time"

	conf "github.com/alibaba/MongoShake/v2/collector/configure"
	utils "github.com/alibaba/MongoShake/v2/common"

	"fmt"

	"github.com/alibaba/MongoShake/v2/collector/ckpt"
	"github.com/alibaba/MongoShake/v2/collector/filter"
	diskQueue "github.com/vinllen/go-diskqueue"
	LOG "github.com/vinllen/log4go"
	"github.com/vinllen/mgo/bson"
)

const (
	ErrInvalidStartPosition = "resume point may no longer be in the oplog."
)

type EventReader struct {
	// source mongo address url
	src     string
	replset string

	// mongo client
	client *utils.ChangeStreamConn

	// stage of fetch and store oplog
	fetchStage int32
	// disk queue used to store oplog temporarily
	diskQueue     *diskQueue.DiskQueue
	disQueueMutex sync.Mutex // disk queue mutex

	// start at operation time
	startAtOperationTime interface{}

	// event channel
	eventChan    chan *retOplog
	fetcherExist bool
	fetcherLock  sync.Mutex

	firstRead       bool
	diskQueueLastTs bson.MongoTimestamp // the last oplog timestamp in disk queue
}

// NewEventReader creates reader with mongodb url
func NewEventReader(src string, replset string) *EventReader {
	var channelSize = int(float64(BatchSize) * PrefetchPercent)
	return &EventReader{
		src:             src,
		replset:         replset,
		eventChan:       make(chan *retOplog, channelSize),
		firstRead:       true,
		diskQueueLastTs: -1,
	}
}

func (er *EventReader) String() string {
	return fmt.Sprintf("EventReader[src:%s replset:%s]", utils.BlockMongoUrlPassword(er.src, "***"),
		er.replset)
}

func (er *EventReader) Name() string {
	return utils.VarIncrSyncMongoFetchMethodChangeStream
}

// SetQueryTimestampOnEmpty set internal timestamp if
// not exist in this or. initial stage most of the time
func (er *EventReader) SetQueryTimestampOnEmpty(ts interface{}) {
	if er.startAtOperationTime == nil && ts != ckpt.InitCheckpoint {
		LOG.Info("set query timestamp: %v", utils.ExtractTimestampForLog(ts))
		if val, ok := ts.(bson.MongoTimestamp); ok {
			er.startAtOperationTime = utils.TimestampToInt64(val)
		} else if val2, ok := ts.(int64); ok {
			er.startAtOperationTime = val2
		} else {
			// ResumeToken
			er.startAtOperationTime = ts
		}
	}
}

func (er *EventReader) UpdateQueryTimestamp(ts bson.MongoTimestamp) {
	er.startAtOperationTime = utils.TimestampToInt64(ts)
}

func (er *EventReader) getQueryTimestamp() bson.MongoTimestamp {
	return bson.MongoTimestamp(er.startAtOperationTime.(int64))
}

// Next returns an oplog by raw bytes which is []byte
func (er *EventReader) Next() ([]byte, error) {
	return er.get()
}

func (er *EventReader) get() ([]byte, error) {
	select {
	case ret := <-er.eventChan:
		return ret.log, ret.err
	case <-time.After(time.Second * time.Duration(conf.Options.IncrSyncReaderBufferTime)):
		return nil, TimeoutError
	}
}

// start fetcher if not exist
func (er *EventReader) StartFetcher() {
	if er.fetcherExist == true {
		return
	}

	er.fetcherLock.Lock()
	if er.fetcherExist == false { // double check
		er.fetcherExist = true
		go er.fetcher()
	}
	er.fetcherLock.Unlock()
}

// fetch change stream event tp store disk queue or memory
func (er *EventReader) fetcher() {
	LOG.Info("start fetcher with src[%v] replica-name[%v] query-ts[%v]",
		utils.BlockMongoUrlPassword(er.src, "***"), er.replset,
		utils.ExtractTimestampForLog(er.startAtOperationTime))

	for {
		if err := er.EnsureNetwork(); err != nil {
			er.eventChan <- &retOplog{nil, err}
			continue
		}

		ok, data := er.client.GetNext()
		if !ok {
			err := er.client.CsHandler.Err()
			// no data
			er.client.Close()
			LOG.Error("change stream reader hit the end: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		er.eventChan <- &retOplog{data, nil}
	}
}

func (er *EventReader) EnsureNetwork() error {
	if er.client != nil && er.client.IsNotNil() {
		return nil
	}

	LOG.Info("%s ensure network", er.String())

	if er.client != nil {
		er.client.Close() // close old client
	}

	filterList := filter.NewDocFilterList()
	var err error
	if er.client, err = utils.NewChangeStreamConn(er.src,
		conf.Options.MongoConnectMode,
		conf.Options.IncrSyncChangeStreamWatchFullDocument,
		conf.Options.SpecialSourceDBFlag,
		filterList.IterateFilter,
		er.startAtOperationTime,
		int32(BatchSize),
		conf.Options.SourceDBVersion); err != nil {
		return err
	}

	return nil
}

func (er *EventReader) FetchNewestTimestamp() (interface{}, error) {
	if err := er.EnsureNetwork(); err != nil {
		return "", err
	}

	// non-blocking, and return is useless
	er.client.TryNext()

	return er.client.ResumeToken(), nil
}
