/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

// GBMonth returns the storage rows a driver can price: unit GB/month and a
// price above zero. Rows in other units are ignored, so the type they price
// stays unknown (the offering's price incomplete).
func GBMonth(rows []StoragePrice) []StoragePrice {
	out := make([]StoragePrice, 0, len(rows))
	for _, r := range rows {
		if r.Unit == "GB/month" && r.USD > 0 {
			out = append(out, r)
		}
	}
	return out
}

// HourlyIPv4 converts the IPv4 rows to USD per hour by Keeper region id: an
// "hour" row as it is, a "month" row divided by the driver's hours per month.
// Rows in another unit or without a price above zero are ignored.
func HourlyIPv4(rows []IPv4Price, hoursPerMonth float64) map[string]float64 {
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		if r.USD <= 0 {
			continue
		}
		switch r.Unit {
		case "hour":
			out[r.Region] = r.USD
		case "month":
			out[r.Region] = r.USD / hoursPerMonth
		}
	}
	return out
}
