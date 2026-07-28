package main

// HeaderPin describes one physical pin on the Raspberry Pi 40-pin header.
type HeaderPin struct {
	Pin   int    `json:"pin"`            // physical pin number, 1-40
	Kind  string `json:"kind"`           // "gpio", "power" or "ground"
	Label string `json:"label"`          // what is silkscreened / commonly called
	GPIO  int    `json:"gpio,omitempty"` // BCM number, only for kind == "gpio"
	Note  string `json:"note,omitempty"` // caveat worth showing in the UI
}

// headerPins is the standard 40-pin header of every Pi since the B+, in
// physical pin order. Odd pins run down the left column, even down the right.
var headerPins = []HeaderPin{
	{Pin: 1, Kind: "power", Label: "3V3"},
	{Pin: 2, Kind: "power", Label: "5V"},
	{Pin: 3, Kind: "gpio", Label: "GPIO2", GPIO: 2, Note: "I2C1 SDA, 1.8k pull-up fitted on board"},
	{Pin: 4, Kind: "power", Label: "5V"},
	{Pin: 5, Kind: "gpio", Label: "GPIO3", GPIO: 3, Note: "I2C1 SCL, 1.8k pull-up fitted on board"},
	{Pin: 6, Kind: "ground", Label: "GND"},
	{Pin: 7, Kind: "gpio", Label: "GPIO4", GPIO: 4},
	{Pin: 8, Kind: "gpio", Label: "GPIO14", GPIO: 14, Note: "UART TX by default"},
	{Pin: 9, Kind: "ground", Label: "GND"},
	{Pin: 10, Kind: "gpio", Label: "GPIO15", GPIO: 15, Note: "UART RX by default"},
	{Pin: 11, Kind: "gpio", Label: "GPIO17", GPIO: 17},
	{Pin: 12, Kind: "gpio", Label: "GPIO18", GPIO: 18},
	{Pin: 13, Kind: "gpio", Label: "GPIO27", GPIO: 27},
	{Pin: 14, Kind: "ground", Label: "GND"},
	{Pin: 15, Kind: "gpio", Label: "GPIO22", GPIO: 22},
	{Pin: 16, Kind: "gpio", Label: "GPIO23", GPIO: 23},
	{Pin: 17, Kind: "power", Label: "3V3"},
	{Pin: 18, Kind: "gpio", Label: "GPIO24", GPIO: 24},
	{Pin: 19, Kind: "gpio", Label: "GPIO10", GPIO: 10, Note: "SPI0 MOSI"},
	{Pin: 20, Kind: "ground", Label: "GND"},
	{Pin: 21, Kind: "gpio", Label: "GPIO9", GPIO: 9, Note: "SPI0 MISO"},
	{Pin: 22, Kind: "gpio", Label: "GPIO25", GPIO: 25},
	{Pin: 23, Kind: "gpio", Label: "GPIO11", GPIO: 11, Note: "SPI0 SCLK"},
	{Pin: 24, Kind: "gpio", Label: "GPIO8", GPIO: 8, Note: "SPI0 CE0"},
	{Pin: 25, Kind: "ground", Label: "GND"},
	{Pin: 26, Kind: "gpio", Label: "GPIO7", GPIO: 7, Note: "SPI0 CE1"},
	{Pin: 27, Kind: "gpio", Label: "ID_SD", GPIO: 0, Note: "HAT EEPROM, avoid unless you know the board has no HAT"},
	{Pin: 28, Kind: "gpio", Label: "ID_SC", GPIO: 1, Note: "HAT EEPROM, avoid unless you know the board has no HAT"},
	{Pin: 29, Kind: "gpio", Label: "GPIO5", GPIO: 5},
	{Pin: 30, Kind: "ground", Label: "GND"},
	{Pin: 31, Kind: "gpio", Label: "GPIO6", GPIO: 6},
	{Pin: 32, Kind: "gpio", Label: "GPIO12", GPIO: 12},
	{Pin: 33, Kind: "gpio", Label: "GPIO13", GPIO: 13},
	{Pin: 34, Kind: "ground", Label: "GND"},
	{Pin: 35, Kind: "gpio", Label: "GPIO19", GPIO: 19},
	{Pin: 36, Kind: "gpio", Label: "GPIO16", GPIO: 16},
	{Pin: 37, Kind: "gpio", Label: "GPIO26", GPIO: 26},
	{Pin: 38, Kind: "gpio", Label: "GPIO20", GPIO: 20},
	{Pin: 39, Kind: "ground", Label: "GND"},
	{Pin: 40, Kind: "gpio", Label: "GPIO21", GPIO: 21},
}

// headerGPIOs returns the BCM numbers exposed on the header, lowest first.
func headerGPIOs() []int {
	offsets := make([]int, 0, 28)
	for _, p := range headerPins {
		if p.Kind == "gpio" {
			offsets = append(offsets, p.GPIO)
		}
	}
	for i := 1; i < len(offsets); i++ {
		for j := i; j > 0 && offsets[j] < offsets[j-1]; j-- {
			offsets[j], offsets[j-1] = offsets[j-1], offsets[j]
		}
	}
	return offsets
}

// physicalPinFor maps a BCM number back to its header pin, 0 if not exposed.
func physicalPinFor(gpio int) int {
	for _, p := range headerPins {
		if p.Kind == "gpio" && p.GPIO == gpio {
			return p.Pin
		}
	}
	return 0
}
