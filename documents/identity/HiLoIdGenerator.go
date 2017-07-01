package identity

import(
	documents ".."
	data "../../data"
	"sync/atomic"
	"sync"
	"fmt"
)

type IHiLoGenerator interface{
	GenerateDocumentId(entity interface{}) string
	ReturnUnusedRange()
}

type HiLoIdGenerator struct{
	store documents.DocumentStore
	conventions data.DocumentConvention
	tag, dBName, identityPartsSeparator, prefix, serverTag string
	availableRange RangeValue
	rangesUpdateMutex sync.Mutex
	generators map[string]IHiLoGenerator
}

type MultiTypeHiLoIdGenerator struct{
	generatorsCollection map[string]HiLoIdGenerator
	store documents.DocumentStore
	dBName string
	convention data.DocumentConvention
	generatorMutex sync.Mutex
}

type MultiDatabaseHiLoIdGenerator struct{

}

type RangeValue struct{
	Min, Max, Current int64
}

func NewRangeValue(min int64, max int64) (*RangeValue, error){
	return &RangeValue{min, max, min-1}, nil
}

func NewHiLoIdGenerator(tag string, store documents.DocumentStore, dBName string, identityPartsSeparator string) (*HiLoIdGenerator, error){
	rangeVal, _ := NewRangeValue(1, 0)
	rangesUpdateChan := make(chan RangeValue, 1)
	return &HiLoIdGenerator{store: store, tag: tag, dBName: dBName, identityPartsSeparator: identityPartsSeparator, availableRange: *rangeVal, rangesUpdateChan: rangesUpdateChan}, nil
}

func NewMultiTypeHiLoIdGenerator(store documents.DocumentStore, dBName string, convention data.DocumentConvention) (*MultiTypeHiLoIdGenerator, error){
	return &MultiTypeHiLoIdGenerator{store: store, dBName: dBName, convention: convention}, nil
}

//Thread safe Id generation
func (generator HiLoIdGenerator) GenerateDocumentId(entity interface{}) int64{
	for{
		id := atomic.AddInt64(&generator.availableRange.Current, 1)
		if id <= generator.availableRange.Max{
			return id
		}else {
			generator.getNextRangeIfNecessary()
		}
	}
}

//Thread safe range check and update if it is needed
func (generator HiLoIdGenerator) getNextRangeIfNecessary(){
	generator.rangesUpdateMutex.Lock()
	if generator.availableRange.Current > generator.availableRange.Max {
		generator.availableRange = generator.getNextRange()
	}
	generator.rangesUpdateMutex.Unlock()
}

//NOT thread safe range fetch
func (generator HiLoIdGenerator) getNextRange() RangeValue{
	//todo after HiLoCommand
}

func (generator HiLoIdGenerator) ReturnUnusedRange(){
	//todo after HiLoCommand
}

func (generator HiLoIdGenerator) CreateDocumentIdFromId(id int64) string{
	return fmt.Sprintf("%s%d-%s", generator.prefix, id, generator.serverTag)
}

//Thread safe document id generator
func (multiGenerator MultiTypeHiLoIdGenerator) GenerateDocumentId(entity interface{}) string{
	typeTagName := multiGenerator.convention.GetCollectionName(entity)
	if typeTagName{
		return "" //todo is it error condition?
	}
	tag := multiGenerator.convention.TypeCollectionNameToDocumentIdPrefixTransformer(typeTagName)
	generator, ok := multiGenerator.generatorsCollection[tag]
	if !ok{
		multiGenerator.generatorMutex.Lock()
		generator, ok := multiGenerator.generatorsCollection[tag]
		if !ok{
			generatorPtr, _ := NewHiLoIdGenerator(tag, generator.store, generator.dBName, generator.conventions.IdentityPartsSeparator)
			multiGenerator.generatorsCollection[tag] = *generatorPtr
			generator = multiGenerator.generatorsCollection[tag]
		}
		multiGenerator.generatorMutex.Unlock()
	}
	return generator.CreateDocumentIdFromId(generator.GenerateDocumentId(entity))
}

func (multiGenerator MultiTypeHiLoIdGenerator) ReturnUnusedRange(){
	for _, generator := range multiGenerator.generatorsCollection{
		generator.ReturnUnusedRange()
	}
}
