package cgv

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	seatmappb "github.com/cineko-org/contracts/gen/go/cineko/seatmap"
)

type seatDataEnvelope struct {
	StatusCode int    `json:"statusCode"`
	ResultMsg  string `json:"resultMsg"`
	Data       struct {
		Items []seatDataItem `json:"items"`
	} `json:"data"`
}

type seatDataItem struct {
	Board     seatDataBoard      `json:"sbord"`
	SaleForms []seatDataSaleForm `json:"salfrms"`
	Zones     []seatDataZone     `json:"szones"`
	Blocks    []seatDataBlock    `json:"sblcks"`
	Seats     []seatDataSeat     `json:"seats"`
}

type seatDataBoard struct {
	XStart string `json:"xcoordStartVal"`
	YStart string `json:"ycoordStartVal"`
	XEnd   string `json:"xcoordEndVal"`
	YEnd   string `json:"ycoordEndVal"`
	Count  int    `json:"stcnt"`
}

type seatDataSaleForm struct {
	Code string `json:"seatSalfrmCd"`
	Name string `json:"seatSalfrmNm"`
}

type seatDataZone struct {
	Code     string `json:"szoneNo"`
	Name     string `json:"szoneNm"`
	KindCode string `json:"szoneKindCd"`
	KindName string `json:"szoneKindNm"`
	XStart   string `json:"xcoordStartVal"`
	YStart   string `json:"ycoordStartVal"`
	XEnd     string `json:"xcoordEndVal"`
	YEnd     string `json:"ycoordEndVal"`
	Capacity string `json:"maxNopsn"`
}

type seatDataBlock struct {
	Code     string `json:"sblckNo"`
	Name     string `json:"sblckNm"`
	KindCode string `json:"sblckKindCd"`
	KindName string `json:"sblckKindNm"`
	XStart   string `json:"xcoordStartVal"`
	YStart   string `json:"ycoordStartVal"`
	XEnd     string `json:"xcoordEndVal"`
	YEnd     string `json:"ycoordEndVal"`
}

type seatDataSeat struct {
	LocationID string `json:"seatLocNo"`
	Row        string `json:"seatRowNm"`
	Number     string `json:"seatNo"`
	KindCode   string `json:"stkndCd"`
	KindName   string `json:"stkndNm"`
	ZoneName   string `json:"szoneNm"`
	ZoneKind   string `json:"szoneKindNm"`
	SaleForm   string `json:"seatSalfrmCd"`
	StatusCode string `json:"seatStusCd"`
	StatusName string `json:"seatStusNm"`
	SaleYN     string `json:"seatSaleYn"`
	XStart     string `json:"xcoordStartVal"`
	YStart     string `json:"ycoordStartVal"`
	XEnd       string `json:"xcoordEndVal"`
	YEnd       string `json:"ycoordEndVal"`
	LeftAisle  string `json:"leftPwayYn"`
	RightAisle string `json:"rghtPwayYn"`
}

// parseSeatMapLayout converts the provider coordinate system into the stable
// normalized layout shared by every Cineko reporter.
func parseSeatMapLayout(body []byte, auditoriumID string) (*seatmappb.Layout, error) {
	var envelope seatDataEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode CGV seat map: %w", err)
	}
	if envelope.StatusCode != 0 {
		return nil, fmt.Errorf("CGV seat map failed: %s", envelope.ResultMsg)
	}
	if len(envelope.Data.Items) == 0 {
		return nil, errors.New("CGV seat map contained no layout")
	}
	layout := &seatmappb.Layout{}
	labels := make(map[string]struct{})
	for _, item := range envelope.Data.Items {
		if err := appendSeatMapItem(layout, item, auditoriumID, labels); err != nil {
			return nil, err
		}
	}
	canonicalizeSeatMapLayout(layout)
	seats := layout.GetSeats()
	zones := layout.GetZones()
	blocks := layout.GetBlocks()
	sort.Slice(seats, func(i, j int) bool { return seats[i].GetLabel() < seats[j].GetLabel() })
	sort.Slice(zones, func(i, j int) bool {
		if zones[i].GetCode() == zones[j].GetCode() {
			return zones[i].GetName() < zones[j].GetName()
		}
		return zones[i].GetCode() < zones[j].GetCode()
	})
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].GetCode() == blocks[j].GetCode() {
			return blocks[i].GetName() < blocks[j].GetName()
		}
		return blocks[i].GetCode() < blocks[j].GetCode()
	})
	layout.SetSeats(seats)
	layout.SetZones(zones)
	layout.SetBlocks(blocks)
	if len(seats) == 0 {
		return nil, errors.New("CGV seat map contained no seats")
	}
	return layout, nil
}

// canonicalizeSeatMapLayout matches Central's static-layout normalization
// before either side computes the deterministic protobuf hash.
func canonicalizeSeatMapLayout(layout *seatmappb.Layout) {
	for _, seat := range layout.GetSeats() {
		if seat == nil {
			continue
		}
		seat.SetAuditoriumId(strings.TrimSpace(seat.GetAuditoriumId()))
		seat.SetLabel(strings.TrimSpace(seat.GetLabel()))
		seat.SetRow(strings.TrimSpace(seat.GetRow()))
		seat.SetType(strings.TrimSpace(seat.GetType()))
		seat.SetZoneName(strings.TrimSpace(seat.GetZoneName()))
		seat.SetZoneKind(strings.TrimSpace(seat.GetZoneKind()))
		seat.SetSaleFormCode(strings.TrimSpace(seat.GetSaleFormCode()))
		seat.SetSaleFormName(strings.TrimSpace(seat.GetSaleFormName()))
		seat.SetFeatures(canonicalSeatMapStrings(seat.GetFeatures()))
		seat.SetSourceLabel(strings.TrimSpace(seat.GetSourceLabel()))
		seat.SetSourceSeatKindCode(strings.TrimSpace(seat.GetSourceSeatKindCode()))
		seat.SetSourceSeatKindName(strings.TrimSpace(seat.GetSourceSeatKindName()))
		seat.SetSourceClasses(canonicalSeatMapStrings(seat.GetSourceClasses()))
	}
	for _, zone := range layout.GetZones() {
		if zone == nil {
			continue
		}
		zone.SetCode(strings.TrimSpace(zone.GetCode()))
		zone.SetName(strings.TrimSpace(zone.GetName()))
		zone.SetKindCode(strings.TrimSpace(zone.GetKindCode()))
		zone.SetKindName(strings.TrimSpace(zone.GetKindName()))
	}
	for _, block := range layout.GetBlocks() {
		if block == nil {
			continue
		}
		block.SetCode(strings.TrimSpace(block.GetCode()))
		block.SetName(strings.TrimSpace(block.GetName()))
		block.SetKindCode(strings.TrimSpace(block.GetKindCode()))
		block.SetKindName(strings.TrimSpace(block.GetKindName()))
	}
}

func canonicalSeatMapStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}

func appendSeatMapItem(
	layout *seatmappb.Layout,
	item seatDataItem,
	auditoriumID string,
	labels map[string]struct{},
) error {
	board, err := newSeatCoordinateBoard(item.Board)
	if err != nil {
		return err
	}
	saleForms := make(map[string]string, len(item.SaleForms))
	for _, saleForm := range item.SaleForms {
		saleForms[saleForm.Code] = saleForm.Name
	}
	if err := appendSeatMapSeats(layout, item, auditoriumID, labels, board, saleForms); err != nil {
		return err
	}
	if err := appendSeatMapZones(layout, item.Zones, board); err != nil {
		return err
	}
	return appendSeatMapBlocks(layout, item.Blocks, board)
}

func appendSeatMapSeats(
	layout *seatmappb.Layout,
	item seatDataItem,
	auditoriumID string,
	labels map[string]struct{},
	board seatCoordinateBoard,
	saleForms map[string]string,
) error {
	start := len(layout.GetSeats())
	for _, source := range item.Seats {
		label, row, number, err := normalizedSeatLabel(source)
		if err != nil {
			return err
		}
		if _, duplicate := labels[label]; duplicate {
			return fmt.Errorf("duplicate CGV seat label %s", label)
		}
		labels[label] = struct{}{}
		saleFormName := saleForms[source.SaleForm]
		x, err := board.centerX(source.XStart, source.XEnd)
		if err != nil {
			return fmt.Errorf("parse CGV seat %s x coordinate: %w", label, err)
		}
		y, err := board.centerY(source.YStart, source.YEnd)
		if err != nil {
			return fmt.Errorf("parse CGV seat %s y coordinate: %w", label, err)
		}
		seat := &seatmappb.Seat{}
		seat.SetId(SeatID(auditoriumID, label))
		seat.SetAuditoriumId(auditoriumID)
		seat.SetLabel(label)
		seat.SetRow(row)
		seatNumber, err := seatNumberAsInt32(number)
		if err != nil {
			return fmt.Errorf("parse CGV seat %s number: %w", label, err)
		}
		seat.SetNumber(seatNumber)
		seat.SetX(x)
		seat.SetY(y)
		seat.SetType(seatMapSeatType(source.KindName, saleFormName))
		seat.SetZoneName(source.ZoneName)
		seat.SetZoneKind(source.ZoneKind)
		seat.SetSaleFormCode(source.SaleForm)
		seat.SetSaleFormName(saleFormName)
		seat.SetLeftAisle(source.LeftAisle == "Y")
		seat.SetRightAisle(source.RightAisle == "Y")
		seat.SetFeatures(seatMapFeatures(source, saleFormName))
		seat.SetSourceLabel(label)
		seat.SetSourceSeatKindCode(source.KindCode)
		seat.SetSourceSeatKindName(source.KindName)
		layout.SetSeats(append(layout.GetSeats(), seat))
	}
	if item.Board.Count > 0 && item.Board.Count != len(layout.GetSeats())-start {
		return fmt.Errorf("CGV board count %d differs from parsed seat count", item.Board.Count)
	}
	return nil
}

func seatNumberAsInt32(value int64) (int32, error) {
	if value < 1 || value > math.MaxInt32 {
		return 0, errors.New("seat number is outside int32 range")
	}
	return int32(value), nil //nolint:gosec // the explicit range check bounds the conversion
}

func normalizedSeatLabel(source seatDataSeat) (string, string, int64, error) {
	number, err := strconv.ParseInt(strings.TrimSpace(source.Number), 10, 32)
	if err != nil || number < 1 {
		return "", "", 0, fmt.Errorf("parse seat %s%s number", source.Row, source.Number)
	}
	row := strings.ToUpper(strings.TrimSpace(source.Row))
	if row == "" {
		return "", "", 0, errors.New("CGV seat map contains an empty seat row")
	}
	return row + strconv.FormatInt(number, 10), row, number, nil
}

func appendSeatMapZones(
	layout *seatmappb.Layout,
	sources []seatDataZone,
	board seatCoordinateBoard,
) error {
	for _, source := range sources {
		capacity, err := parseOptionalCapacity(source.Capacity)
		if err != nil {
			return fmt.Errorf("parse CGV zone %s capacity: %w", source.Code, err)
		}
		minX, maxX, minY, maxY, err := board.bounds(source.XStart, source.XEnd, source.YStart, source.YEnd)
		if err != nil {
			return fmt.Errorf("parse CGV zone %s bounds: %w", source.Code, err)
		}
		zone := &seatmappb.LayoutZone{}
		zone.SetCode(source.Code)
		zone.SetName(source.Name)
		zone.SetKindCode(source.KindCode)
		zone.SetKindName(source.KindName)
		zone.SetMinX(minX)
		zone.SetMaxX(maxX)
		zone.SetMinY(minY)
		zone.SetMaxY(maxY)
		zone.SetCapacity(capacity)
		layout.SetZones(append(layout.GetZones(), zone))
	}
	return nil
}

func appendSeatMapBlocks(
	layout *seatmappb.Layout,
	sources []seatDataBlock,
	board seatCoordinateBoard,
) error {
	for _, source := range sources {
		minX, maxX, minY, maxY, err := board.bounds(source.XStart, source.XEnd, source.YStart, source.YEnd)
		if err != nil {
			return fmt.Errorf("parse CGV block %s bounds: %w", source.Code, err)
		}
		block := &seatmappb.LayoutBlock{}
		block.SetCode(source.Code)
		block.SetName(source.Name)
		block.SetKindCode(source.KindCode)
		block.SetKindName(source.KindName)
		block.SetMinX(minX)
		block.SetMaxX(maxX)
		block.SetMinY(minY)
		block.SetMaxY(maxY)
		layout.SetBlocks(append(layout.GetBlocks(), block))
	}
	return nil
}

type seatCoordinateBoard struct{ minX, maxX, minY, maxY float64 }

func newSeatCoordinateBoard(source seatDataBoard) (seatCoordinateBoard, error) {
	minX, err := seatCoordinate(source.XStart)
	if err != nil {
		return seatCoordinateBoard{}, fmt.Errorf("parse CGV board x start: %w", err)
	}
	maxX, err := seatCoordinate(source.XEnd)
	if err != nil {
		return seatCoordinateBoard{}, fmt.Errorf("parse CGV board x end: %w", err)
	}
	minY, err := seatCoordinate(source.YStart)
	if err != nil {
		return seatCoordinateBoard{}, fmt.Errorf("parse CGV board y start: %w", err)
	}
	maxY, err := seatCoordinate(source.YEnd)
	if err != nil {
		return seatCoordinateBoard{}, fmt.Errorf("parse CGV board y end: %w", err)
	}
	board := seatCoordinateBoard{minX: minX, maxX: maxX, minY: minY, maxY: maxY}
	if board.maxX <= board.minX || board.maxY <= board.minY {
		return seatCoordinateBoard{}, errors.New("CGV seat map has invalid board bounds")
	}
	return board, nil
}

func (board seatCoordinateBoard) x(value string) (float64, error) {
	parsed, err := seatCoordinate(value)
	if err != nil {
		return 0, err
	}
	return normalizeSeatAxis(parsed, board.minX, board.maxX)
}

func (board seatCoordinateBoard) y(value string) (float64, error) {
	parsed, err := seatCoordinate(value)
	if err != nil {
		return 0, err
	}
	return normalizeSeatAxis(parsed, board.minY, board.maxY)
}

func (board seatCoordinateBoard) centerX(start, end string) (float64, error) {
	return board.center(start, end, board.minX, board.maxX)
}

func (board seatCoordinateBoard) centerY(start, end string) (float64, error) {
	return board.center(start, end, board.minY, board.maxY)
}

func (board seatCoordinateBoard) center(start, end string, minimum, maximum float64) (float64, error) {
	startValue, err := seatCoordinate(start)
	if err != nil {
		return 0, err
	}
	endValue, err := seatCoordinate(end)
	if err != nil {
		return 0, err
	}
	if endValue < startValue {
		return 0, errors.New("coordinate end precedes start")
	}
	return normalizeSeatAxis((startValue+endValue)/2, minimum, maximum)
}

func (board seatCoordinateBoard) bounds(xStart, xEnd, yStart, yEnd string) (float64, float64, float64, float64, error) {
	minX, err := board.x(xStart)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	maxX, err := board.x(xEnd)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	minY, err := board.y(yStart)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	maxY, err := board.y(yEnd)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if minX > maxX || minY > maxY {
		return 0, 0, 0, 0, errors.New("coordinate end precedes start")
	}
	return minX, maxX, minY, maxY, nil
}

func seatCoordinate(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, errors.New("coordinate is not finite")
	}
	return parsed, nil
}

func normalizeSeatAxis(value, minimum, maximum float64) (float64, error) {
	normalized := (value - minimum) / (maximum - minimum)
	if normalized < 0 || normalized > 1 || math.IsNaN(normalized) || math.IsInf(normalized, 0) {
		return 0, errors.New("coordinate is outside board bounds")
	}
	return normalized, nil
}

func parseOptionalCapacity(value string) (int32, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	capacity, err := strconv.ParseInt(value, 10, 32)
	if err != nil || capacity < 0 {
		return 0, errors.New("capacity is invalid")
	}
	return int32(capacity), nil
}

func seatMapSeatType(kindName, saleFormName string) string {
	haystack := strings.ToLower(kindName + " " + saleFormName)
	for _, candidate := range []struct {
		keywords []string
		value    string
	}{
		{[]string{"wheel", "휠체어", "이동식"}, "wheelchair"},
		{[]string{"companion", "보호자"}, "companion"},
		{[]string{"couple", "sweetbox", "커플"}, "couple"},
		{[]string{"tempur", "bed", "템퍼"}, "bed"},
		{[]string{"recliner", "stressless", "리클라이너"}, "recliner"},
		{[]string{"premium", "primium", "프리미엄"}, "premium"},
		{[]string{"prime", "프라임"}, "prime"},
		{[]string{"4dx", "motion", "모션"}, "motion"},
	} {
		for _, keyword := range candidate.keywords {
			if strings.Contains(haystack, keyword) {
				return candidate.value
			}
		}
	}
	return "standard"
}

func seatMapFeatures(source seatDataSeat, saleFormName string) []string {
	features := make([]string, 0, 5)
	if zoneName := strings.TrimSpace(source.ZoneName); zoneName != "" {
		features = append(features, "zone:"+zoneName)
	}
	if saleFormName = strings.TrimSpace(saleFormName); saleFormName != "" {
		features = append(features, "sale-form:"+saleFormName)
	}
	if source.LeftAisle == "Y" {
		features = append(features, "left-aisle")
	}
	if source.RightAisle == "Y" {
		features = append(features, "right-aisle")
	}
	if strings.Contains(saleFormName, "이동식") {
		features = append(features, "wheelchair-area", "removable")
	}
	return features
}
