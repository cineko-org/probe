package contracts

// SeatMapLayout is the canonical static layout stored by Central. Live sale
// status is deliberately excluded because it belongs to a showtime observation.
type SeatMapLayout struct {
	Seats  []SeatMapSeat `json:"seats"`
	Zones  []LayoutZone  `json:"zones"`
	Blocks []LayoutBlock `json:"blocks"`
}

const (
	SeatMapResolutionReady   = "ready"
	SeatMapResolutionWaiting = "waiting"
)

// SeatMapResolution is Central's complete response to a Client seat-map
// request. Clients do not participate in cache or capture decisions.
type SeatMapResolution struct {
	Status  string          `json:"status"`
	SeatMap *SeatMapVersion `json:"seatMap,omitempty"`
}

// SeatMapSeat describes one physical seat and its normalized auditorium position.
type SeatMapSeat struct {
	ID                 string   `json:"id"`
	AuditoriumID       string   `json:"auditoriumId"`
	Label              string   `json:"label"`
	Row                string   `json:"row"`
	Number             int      `json:"number"`
	X                  float64  `json:"x"`
	Y                  float64  `json:"y"`
	Type               string   `json:"type"`
	ZoneName           string   `json:"zoneName"`
	ZoneKind           string   `json:"zoneKind"`
	SaleFormCode       string   `json:"saleFormCode"`
	SaleFormName       string   `json:"saleFormName"`
	LeftAisle          bool     `json:"leftAisle"`
	RightAisle         bool     `json:"rightAisle"`
	Features           []string `json:"features"`
	SourceLabel        string   `json:"sourceLabel"`
	SourceSeatKindCode string   `json:"sourceSeatKindCode"`
	SourceSeatKindName string   `json:"sourceSeatKindName"`
	SourceClasses      []string `json:"sourceClasses,omitempty"`
}

// LayoutZone preserves a provider-defined pricing or seating zone.
type LayoutZone struct {
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	KindCode string  `json:"kindCode"`
	KindName string  `json:"kindName"`
	MinX     float64 `json:"minX"`
	MaxX     float64 `json:"maxX"`
	MinY     float64 `json:"minY"`
	MaxY     float64 `json:"maxY"`
	Capacity int     `json:"capacity"`
}

// LayoutBlock preserves a provider-defined physical seating block.
type LayoutBlock struct {
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	KindCode string  `json:"kindCode"`
	KindName string  `json:"kindName"`
	MinX     float64 `json:"minX"`
	MaxX     float64 `json:"maxX"`
	MinY     float64 `json:"minY"`
	MaxY     float64 `json:"maxY"`
}
