package cgv

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	contracts "github.com/cineko-org/contracts/v3"
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
	Row        string `json:"seatRowNm"`
	Number     string `json:"seatNo"`
	KindCode   string `json:"stkndCd"`
	KindName   string `json:"stkndNm"`
	ZoneName   string `json:"szoneNm"`
	ZoneKind   string `json:"szoneKindNm"`
	SaleForm   string `json:"seatSalfrmCd"`
	XStart     string `json:"xcoordStartVal"`
	YStart     string `json:"ycoordStartVal"`
	XEnd       string `json:"xcoordEndVal"`
	YEnd       string `json:"ycoordEndVal"`
	LeftAisle  string `json:"leftPwayYn"`
	RightAisle string `json:"rghtPwayYn"`
}

// parseSeatMapLayout converts the provider coordinate system into the stable
// normalized layout shared by every Cineko reporter.
func parseSeatMapLayout(body []byte, auditoriumID string) (contracts.SeatMapLayout, error) {
	var envelope seatDataEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return contracts.SeatMapLayout{}, fmt.Errorf("decode CGV seat map: %w", err)
	}
	if envelope.StatusCode != 0 {
		return contracts.SeatMapLayout{}, fmt.Errorf("CGV seat map failed: %s", envelope.ResultMsg)
	}
	if len(envelope.Data.Items) == 0 {
		return contracts.SeatMapLayout{}, errors.New("CGV seat map contained no layout")
	}
	layout := contracts.SeatMapLayout{}
	labels := make(map[string]struct{})
	for _, item := range envelope.Data.Items {
		if err := appendSeatMapItem(&layout, item, auditoriumID, labels); err != nil {
			return contracts.SeatMapLayout{}, err
		}
	}
	sort.Slice(layout.Seats, func(i, j int) bool { return layout.Seats[i].Label < layout.Seats[j].Label })
	sort.Slice(layout.Zones, func(i, j int) bool {
		if layout.Zones[i].Code == layout.Zones[j].Code {
			return layout.Zones[i].Name < layout.Zones[j].Name
		}
		return layout.Zones[i].Code < layout.Zones[j].Code
	})
	sort.Slice(layout.Blocks, func(i, j int) bool {
		if layout.Blocks[i].Code == layout.Blocks[j].Code {
			return layout.Blocks[i].Name < layout.Blocks[j].Name
		}
		return layout.Blocks[i].Code < layout.Blocks[j].Code
	})
	if len(layout.Seats) == 0 {
		return contracts.SeatMapLayout{}, errors.New("CGV seat map contained no seats")
	}
	return layout, nil
}

func appendSeatMapItem(
	layout *contracts.SeatMapLayout,
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
	layout *contracts.SeatMapLayout,
	item seatDataItem,
	auditoriumID string,
	labels map[string]struct{},
	board seatCoordinateBoard,
	saleForms map[string]string,
) error {
	start := len(layout.Seats)
	for _, source := range item.Seats {
		number, err := strconv.Atoi(source.Number)
		if err != nil || number < 1 {
			return fmt.Errorf("parse seat %s%s number", source.Row, source.Number)
		}
		row := strings.ToUpper(strings.TrimSpace(source.Row))
		if row == "" {
			return errors.New("CGV seat map contains an empty seat row")
		}
		label := row + strconv.Itoa(number)
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
		layout.Seats = append(layout.Seats, contracts.SeatMapSeat{
			ID: contracts.SeatID(auditoriumID, label), AuditoriumID: auditoriumID,
			Label: label, Row: row, Number: number,
			X: x, Y: y,
			Type:     seatMapSeatType(source.KindName, saleFormName),
			ZoneName: source.ZoneName, ZoneKind: source.ZoneKind,
			SaleFormCode: source.SaleForm, SaleFormName: saleFormName,
			LeftAisle: source.LeftAisle == "Y", RightAisle: source.RightAisle == "Y",
			Features: seatMapFeatures(source, saleFormName), SourceLabel: label,
			SourceSeatKindCode: source.KindCode, SourceSeatKindName: source.KindName,
		})
	}
	if item.Board.Count > 0 && item.Board.Count != len(layout.Seats)-start {
		return fmt.Errorf("CGV board count %d differs from parsed seat count", item.Board.Count)
	}
	return nil
}

func appendSeatMapZones(
	layout *contracts.SeatMapLayout,
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
		layout.Zones = append(layout.Zones, contracts.LayoutZone{
			Code: source.Code, Name: source.Name, KindCode: source.KindCode, KindName: source.KindName,
			MinX: minX, MaxX: maxX, MinY: minY, MaxY: maxY, Capacity: capacity,
		})
	}
	return nil
}

func appendSeatMapBlocks(
	layout *contracts.SeatMapLayout,
	sources []seatDataBlock,
	board seatCoordinateBoard,
) error {
	for _, source := range sources {
		minX, maxX, minY, maxY, err := board.bounds(source.XStart, source.XEnd, source.YStart, source.YEnd)
		if err != nil {
			return fmt.Errorf("parse CGV block %s bounds: %w", source.Code, err)
		}
		layout.Blocks = append(layout.Blocks, contracts.LayoutBlock{
			Code: source.Code, Name: source.Name, KindCode: source.KindCode, KindName: source.KindName,
			MinX: minX, MaxX: maxX, MinY: minY, MaxY: maxY,
		})
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

func parseOptionalCapacity(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	capacity, err := strconv.Atoi(value)
	if err != nil || capacity < 0 {
		return 0, errors.New("capacity is invalid")
	}
	return capacity, nil
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
