package ravendb

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	_ IDatabaseChanges = &DatabaseChanges{}
)

type DatabaseChanges struct {
	_commandId int // TODO: make atomic?

	// TODO: why semaphore of size 1 and not a mutex?
	_semaphore chan bool

	_requestExecutor *RequestExecutor
	_conventions     *DocumentConventions
	_database        string

	_onDispose Runnable

	_client    *websocket.Conn
	_processor *WebSocketChangesProcessor

	_task *CompletableFuture
	_cts  *CancellationTokenSource
	_tcs  *CompletableFuture

	mu             sync.Mutex // protects _confirmations and _counters maps
	_confirmations map[int]*CompletableFuture
	_counters      map[string]*DatabaseConnectionState

	_immediateConnection atomicInteger

	_connectionStatusEventHandlerIdx int
	_connectionStatusChanged         []func()
	onError                          []func(error)
}

func NewDatabaseChanges(requestExecutor *RequestExecutor, databaseName string, onDispose Runnable) *DatabaseChanges {
	res := &DatabaseChanges{
		_requestExecutor:                 requestExecutor,
		_conventions:                     requestExecutor.GetConventions(),
		_database:                        databaseName,
		_tcs:                             NewCompletableFuture(),
		_cts:                             NewCancellationTokenSource(),
		_onDispose:                       onDispose,
		_semaphore:                       make(chan bool, 1),
		_connectionStatusEventHandlerIdx: -1,
	}

	res._task = NewCompletableFuture()
	go func() {
		err := res.doWork()
		if err != nil {
			res._task.CompleteExceptionally(err)
		} else {
			res._task.Complete(nil)
		}
	}()

	_connectionStatusEventHandler := func() {
		res.onConnectionStatusChanged()
	}
	res._connectionStatusEventHandlerIdx = res.addConnectionStatusChanged(_connectionStatusEventHandler)
	return res
}

func (c *DatabaseChanges) onConnectionStatusChanged() {
	c._semaphore <- true // acquire
	defer func() {
		<-c._semaphore // release
	}()

	if c.isConnected() {
		c._tcs.Complete(c)
		return
	}

	if c._tcs.IsDone() {
		c._tcs = NewCompletableFuture()
	}
}

func (c *DatabaseChanges) isConnected() bool {
	return c._client != nil
}

func (c *DatabaseChanges) ensureConnectedNow() error {
	_, err := c._tcs.Get()
	return err
}

func (c *DatabaseChanges) addConnectionStatusChanged(handler func()) int {
	idx := len(c._connectionStatusChanged)
	c._connectionStatusChanged = append(c._connectionStatusChanged, handler)
	return idx

}

func (c *DatabaseChanges) removeConnectionStatusChanged(handlerIdx int) {
	if handlerIdx != -1 {
		c._connectionStatusChanged[handlerIdx] = nil
	}
}

func (c *DatabaseChanges) forIndex(indexName string) (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("indexes/"+indexName, "watch-index", "unwatch-index", indexName)
	if err != nil {
		return nil, err
	}

	filter := func(notification interface{}) bool {
		v := notification.(*IndexChange)
		return strings.EqualFold(v.Name, indexName)
	}

	taskedObservable := NewChangesObservable(ChangesType_INDEX, counter, filter)
	return taskedObservable, nil
}

func (c *DatabaseChanges) getLastConnectionStateException() error {
	for _, counter := range c._counters {
		valueLastException := counter.lastException
		if valueLastException != nil {
			return valueLastException
		}
	}
	return nil
}

func (c *DatabaseChanges) forDocument(docId string) (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("docs/"+docId, "watch-doc", "unwatch-doc", docId)
	if err != nil {
		return nil, err
	}

	filter := func(notification interface{}) bool {
		v := notification.(*DocumentChange)
		return strings.EqualFold(v.ID, docId)
	}
	taskedObservable := NewChangesObservable(ChangesType_DOCUMENT, counter, filter)
	return taskedObservable, nil
}

func filterAlwaysTrue(notification interface{}) bool {
	return true
}

func (c *DatabaseChanges) forAllDocuments() (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("all-docs", "watch-docs", "unwatch-docs", "")
	if err != nil {
		return nil, err
	}
	taskedObservable := NewChangesObservable(ChangesType_DOCUMENT, counter, filterAlwaysTrue)
	return taskedObservable, nil
}

func (c *DatabaseChanges) forOperationId(operationId int) (IChangesObservable, error) {
	opIDStr := strconv.Itoa(operationId)
	counter, err := c.getOrAddConnectionState("operations/"+opIDStr, "watch-operation", "unwatch-operation", opIDStr)
	if err != nil {
		return nil, err
	}

	filter := func(notification interface{}) bool {
		v := notification.(*OperationStatusChange)
		return v.OperationID == operationId
	}
	taskedObservable := NewChangesObservable(ChangesType_OPERATION, counter, filter)
	return taskedObservable, nil
}

func (c *DatabaseChanges) forAllOperations() (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("all-operations", "watch-operations", "unwatch-operations", "")
	if err != nil {
		return nil, err
	}

	taskedObservable := NewChangesObservable(ChangesType_OPERATION, counter, filterAlwaysTrue)

	return taskedObservable, nil
}

func (c *DatabaseChanges) forAllIndexes() (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("all-indexes", "watch-indexes", "unwatch-indexes", "")
	if err != nil {
		return nil, err
	}

	taskedObservable := NewChangesObservable(ChangesType_INDEX, counter, filterAlwaysTrue)

	return taskedObservable, nil
}

func (c *DatabaseChanges) forDocumentsStartingWith(docIdPrefix string) (IChangesObservable, error) {
	counter, err := c.getOrAddConnectionState("prefixes/"+docIdPrefix, "watch-prefix", "unwatch-prefix", docIdPrefix)
	if err != nil {
		return nil, err
	}
	filter := func(notification interface{}) bool {
		v := notification.(*DocumentChange)
		n := len(docIdPrefix)
		if n > len(v.ID) {
			return false
		}
		prefix := v.ID[:n]
		return strings.EqualFold(prefix, docIdPrefix)
	}

	taskedObservable := NewChangesObservable(ChangesType_DOCUMENT, counter, filter)

	return taskedObservable, nil
}

func (c *DatabaseChanges) forDocumentsInCollection(collectionName string) (IChangesObservable, error) {
	if collectionName == "" {
		return nil, NewIllegalArgumentException("CollectionName cannot be empty")
	}

	counter, err := c.getOrAddConnectionState("collections/"+collectionName, "watch-collection", "unwatch-collection", collectionName)
	if err != nil {
		return nil, err
	}

	filter := func(notification interface{}) bool {
		v := notification.(*DocumentChange)
		return strings.EqualFold(collectionName, v.CollectionName)
	}

	taskedObservable := NewChangesObservable(ChangesType_DOCUMENT, counter, filter)

	return taskedObservable, nil
}

/*
func (c *DatabaseChanges) forDocumentsInCollection(Class<?> clazz) (IChangesObservable, error) {
	String collectionName = _conventions.getCollectionName(clazz);
	return forDocumentsInCollection(collectionName);
}
*/

func (c *DatabaseChanges) forDocumentsOfType(typeName string) (IChangesObservable, error) {
	if typeName == "" {
		return nil, NewIllegalArgumentException("TypeName cannot be empty")
	}

	encodedTypeName := UrlUtils_escapeDataString(typeName)

	counter, err := c.getOrAddConnectionState("types/"+typeName, "watch-type", "unwatch-type", encodedTypeName)
	if err != nil {
		return nil, err
	}

	filter := func(notification interface{}) bool {
		v := notification.(*DocumentChange)
		return strings.EqualFold(typeName,
			v.TypeName)
	}

	taskedObservable := NewChangesObservable(ChangesType_DOCUMENT, counter, filter)

	return taskedObservable, nil
}

/*
   public IChangesObservable<DocumentChange> forDocumentsOfType(Class<?> clazz) {
       if (clazz == null) {
           throw new IllegalArgumentException("Clazz cannot be null");
       }

       String className = _conventions.getFindJavaClassName().apply(clazz);
       return forDocumentsOfType(className);
   }

*/

func (c *DatabaseChanges) invokeConnectionStatusChanged() {
	for _, fn := range c._connectionStatusChanged {
		if fn != nil {
			fn()
		}
	}
}

func (c *DatabaseChanges) addOnError(handler func(error)) int {
	idx := len(c.onError)
	c.onError = append(c.onError, handler)
	return idx
}

func (c *DatabaseChanges) removeOnError(handlerIdx int) {
	c.onError[handlerIdx] = nil
}

func (c *DatabaseChanges) invokeOnError(err error) {
	for _, fn := range c.onError {
		if fn != nil {
			fn(err)
		}
	}
}

func (c *DatabaseChanges) Close() {
	c.mu.Lock()
	for _, confirmation := range c._confirmations {
		confirmation.Cancel(false)
	}
	c._semaphore <- true // acquire
	c._client.Close()
	c._client = nil
	<-c._semaphore // release
	c._cts.cancel()
	c._counters = nil
	c.mu.Unlock()
	c._task.Get()
	c.invokeConnectionStatusChanged()
	c.removeConnectionStatusChanged(c._connectionStatusEventHandlerIdx)
	if c._onDispose != nil {
		c._onDispose()
	}
}

func (c *DatabaseChanges) getOrAddConnectionState(name string, watchCommand string, unwatchCommand string, value string) (*DatabaseConnectionState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counter, ok := c._counters[name]
	if ok {
		return counter, nil
	}

	s := name
	onDisconnect := func() {
		if c.isConnected() {
			err := c.send(unwatchCommand, value)
			if err != nil {
				// if we are not connected then we unsubscribed already
				// because connections drops with all subscriptions
			}
		}

		c.mu.Lock()
		state := c._counters[s]
		delete(c._counters, s)
		c.mu.Unlock()
		state.Close()
	}

	onConnect := func() {
		c.send(watchCommand, value)
	}

	counter = NewDatabaseConnectionState(onConnect, onDisconnect)
	c._counters[name] = counter

	if c._immediateConnection.get() == 0 {
		counter.onConnect()
	}
	return counter, nil
}

func (c *DatabaseChanges) send(command, value string) error {
	taskCompletionSource := NewCompletableFuture()
	c._semaphore <- true // acquire

	c._commandId++
	currentCommandId := c._commandId

	o := struct {
		CommandID int    `json:"CommandId"`
		Command   string `json:"Command"`
		Param     string `json:"Param"`
	}{
		CommandID: currentCommandId,
		Command:   command,
		Param:     value,
	}

	err := c._client.WriteJSON(o)
	c._confirmations[currentCommandId] = taskCompletionSource

	<-c._semaphore // release
	if err != nil {
		return err
	}

	_, err = taskCompletionSource.GetWithTimeout(time.Second * 15)
	return err
}

func (c *DatabaseChanges) doWork() error {
	_, err := c._requestExecutor.getPreferredNode()
	if err != nil {
		c.invokeConnectionStatusChanged()
		c.notifyAboutError(err)
		return err
	}
	panic("NYI")
	return nil
}

func (c *DatabaseChanges) reconnectClient() bool {
	if c._cts.getToken().isCancellationRequested() {
		return false
	}

	c._immediateConnection.set(0)

	c.invokeConnectionStatusChanged()
	return true
}

func (c *DatabaseChanges) notifySubscribers(typ string, value interface{}, states []*DatabaseConnectionState) error {
	switch typ {
	case "DocumentChange":
		var documentChange *DocumentChange
		err := decodeJSONAsStruct(value, &documentChange)
		if err != nil {
			return err
		}
		for _, state := range states {
			state.sendDocumentChange(documentChange)
		}
	case "IndexChange":
		var indexChange *IndexChange
		err := decodeJSONAsStruct(value, &indexChange)
		if err != nil {
			return err
		}
		for _, state := range states {
			state.sendIndexChange(indexChange)
		}
	case "OperationStatusChange":
		var operationStatusChange *OperationStatusChange
		err := decodeJSONAsStruct(value, &operationStatusChange)
		if err != nil {
			return err
		}
		for _, state := range states {
			state.sendOperationStatusChange(operationStatusChange)
		}
	default:
		return fmt.Errorf("notifySubscribers: unsupported type '%s'", typ)
	}
	return nil
}

func (c *DatabaseChanges) notifyAboutError(e error) {
	if c._cts.getToken().isCancellationRequested() {
		return
	}

	c.invokeOnError(e)

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c._counters {
		state.error(e)
	}
}

type WebSocketChangesProcessor struct {
	processing *CompletableFuture
	client     *websocket.Conn
}

func (p *WebSocketChangesProcessor) processMessages(changes *DatabaseChanges) {
	var err error
	for {
		var msgArray []interface{} // an array of objects
		err = p.client.ReadJSON(&msgArray)
		if err != nil {
			break
		}
		for _, msgNodeV := range msgArray {
			msgNode := msgNodeV.(map[string]interface{})
			typ, _ := jsonGetAsString(msgNode, "Type")
			switch typ {
			case "Error":
				errStr, _ := jsonGetAsString(msgNode, "Error")
				changes.notifyAboutError(NewRuntimeException("%s", errStr))
			case "Confirm":
				commandID, ok := jsonGetAsInt(msgNode, "CommandId")
				if ok {
					// TODO: protect with semaphore?
					future := changes._confirmations[commandID]
					if future != nil {
						future.Complete(nil)
					}
				}
			default:
				val := msgNode["Value"]
				var states []*DatabaseConnectionState
				for _, state := range changes._counters {
					states = append(states, state)
				}
				changes.notifySubscribers(typ, val, states)
			}

		}
	}
	// TODO: check for io.EOF for clean connection close?
	changes.notifyAboutError(err)
	p.processing.CompleteExceptionally(err)
}
