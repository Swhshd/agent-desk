package services

import "strconv"

func (s *wsService) customerTopic(customerID int64) string {
	if customerID <= 0 {
		return ""
	}
	return "customer:" + strconv.FormatInt(customerID, 10)
}
