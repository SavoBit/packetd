package reports

import (
	"errors"
	"fmt"
	"github.com/untangle/packetd/services/logger"
	"strconv"
	"time"
)

// makeSQLString makes a SQL string from a ReportEntry
func makeSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	if reportEntry.Table == "" {
		return "", errors.New("Missing required attribute Table")
	}

	if reportEntry.Type == "TEXT" {
		return makeTextSQLString(reportEntry, startTime, endTime)
	} else if reportEntry.Type == "EVENTS" {
		return makeEventsSQLString(reportEntry, startTime, endTime)
	} else if reportEntry.Type == "CATEGORIES" {
		return makeCategoriesSQLString(reportEntry, startTime, endTime)
	} else if reportEntry.Type == "SERIES" {
		return makeSeriesSQLString(reportEntry, startTime, endTime)
	} else if reportEntry.Type == "CATEGORIES_SERIES" {
		return makeCategoriesSeriesSQLString(reportEntry, startTime, endTime)
	} else {
		return "", errors.New("Unsupported reportEntry type")
	}
}

// makeTextSQLString makes a SQL string from a TEXT type ReportEntry
func makeTextSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	if reportEntry.QueryText.TextColumns == nil {
		return "", errors.New("Missing required attribute TextColumns")
	}

	sqlStr := "SELECT"
	for i, column := range reportEntry.QueryText.TextColumns {
		if column == "" {
			return "", errors.New("Missing column name")
		}
		if i == 0 {
			sqlStr += " " + column
		} else {
			sqlStr += ", " + column
		}
	}
	sqlStr += " FROM"
	sqlStr += " " + escape(reportEntry.Table)
	sqlStr += " WHERE " + timeStampConditions(startTime, endTime)
	return sqlStr, nil
}

// makeEventsSQLString makes a SQL string from a EVENTS type ReportEntry
func makeEventsSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	sqlStr := "SELECT * FROM"
	sqlStr += " " + escape(reportEntry.Table)
	sqlStr += " WHERE " + timeStampConditions(startTime, endTime)
	return sqlStr, nil
}

// makeCategoriesSQLString makes a SQL string from a CATEGORY type ReportEntry
func makeCategoriesSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	if reportEntry.QueryCategories.CategoriesGroupColumn == "" {
		return "", errors.New("Missing required attribute categoriesGroupColumn")
	}
	if reportEntry.QueryCategories.CategoriesAggregationFunction == "" {
		return "", errors.New("Missing required attribute categoriesAggregationFunction")
	}
	if reportEntry.QueryCategories.CategoriesAggregationValue == "" {
		return "", errors.New("Missing required attribute categoriesAggregationValue")
	}
	var orderByColumn = 2
	if reportEntry.QueryCategories.CategoriesOrderByColumn < 0 || reportEntry.QueryCategories.CategoriesOrderByColumn > 2 {
		return "", errors.New("Illegal value for categoriesOrderByColumn")
	}
	if reportEntry.QueryCategories.CategoriesOrderByColumn != 0 {
		orderByColumn = reportEntry.QueryCategories.CategoriesOrderByColumn
	}
	var order = "DESC"
	if reportEntry.QueryCategories.CategoriesOrderAsc {
		order = "ASC"
	}

	sqlStr := "SELECT"
	sqlStr += " " + reportEntry.QueryCategories.CategoriesGroupColumn
	sqlStr += ", " + reportEntry.QueryCategories.CategoriesAggregationFunction + "(" + reportEntry.QueryCategories.CategoriesAggregationValue + ")"
	sqlStr += " as value"
	sqlStr += " FROM " + escape(reportEntry.Table)
	sqlStr += " WHERE " + timeStampConditions(startTime, endTime)
	sqlStr += " GROUP BY " + reportEntry.QueryCategories.CategoriesGroupColumn
	sqlStr += fmt.Sprintf(" ORDER BY %d %s", orderByColumn, order)

	if reportEntry.QueryCategories.CategoriesLimit != 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", reportEntry.QueryCategories.CategoriesLimit)
	}
	return sqlStr, nil
}

// makeSeriesSQLString makes a SQL string from a SERIES type ReportEntry
func makeSeriesSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	var timeIntervalSec = reportEntry.QuerySeries.SeriesTimeIntervalSeconds
	if timeIntervalSec == 0 {
		timeIntervalSec = 60
	}
	var timeIntervalMilli = int64(timeIntervalSec) * 1000

	tStr, err := makeTimelineSQLString(startTime, endTime, int64(timeIntervalSec))
	if err != nil {
		return "", err
	}

	qStr := "SELECT"
	qStr += fmt.Sprintf(" (time_stamp/%d*%d) as time_trunc", timeIntervalMilli, timeIntervalMilli)
	for _, column := range reportEntry.QuerySeries.SeriesColumns {
		if column == "" {
			return "", errors.New("Missing column name")
		}
		qStr += ", " + column
	}
	qStr += " FROM " + escape(reportEntry.Table)
	qStr += " WHERE " + timeStampConditions(startTime, endTime)
	qStr += " GROUP BY time_trunc"

	sqlStr := "SELECT * FROM "
	sqlStr += " ( " + tStr + " ) as t1 "
	sqlStr += "LEFT JOIN "
	sqlStr += " ( " + qStr + " ) as t2 "
	sqlStr += " USING (time_trunc) "
	sqlStr += " ORDER BY time_trunc DESC "

	return sqlStr, nil
}

// makeCategoriesSeriesSQLString makes a SQL string from a CATEGORIES_SERIES type ReportEntry
func makeCategoriesSeriesSQLString(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) (string, error) {
	if reportEntry.QueryCategories.CategoriesLimit == 0 {
		return "", errors.New("Missing required attribute CategoriesLimit")
	}

	distinctValues, err := getDistinctValues(reportEntry, startTime, endTime)
	logger.Debug("Distinct Values: %v\n", distinctValues)
	if err != nil {
		return "", err
	}

	var columns []string
	for _, column := range distinctValues {
		columnStr := reportEntry.QueryCategories.CategoriesAggregationFunction + "("
		columnStr += "CASE WHEN " + reportEntry.QueryCategories.CategoriesGroupColumn + " = '" + column + "'"
		columnStr += " THEN " + reportEntry.QueryCategories.CategoriesAggregationValue + " END)"
		columns = append(columns, columnStr)
	}

	reportEntry.QuerySeries.SeriesColumns = columns

	return makeSeriesSQLString(reportEntry, startTime, endTime)
}

// return the SQL conditions/fragment to limit the time_stamp
// to the specified startTime and endTime
func timeStampConditions(startTime time.Time, endTime time.Time) string {
	//startTimeStr := startTime.Format("yyyy-MM-dd HH:mm:ss")
	startTimeStr := dateFormat(startTime)
	endTimeStr := dateFormat(endTime)
	return fmt.Sprintf("time_stamp > %s AND time_stamp < %s", startTimeStr, endTimeStr)
}

// escape escapes quotes in as string
// this is a really gross way to handle SQL safety
// https://github.com/golang/go/issues/18478
func escape(source string) string {
	var j int
	if len(source) == 0 {
		return ""
	}
	tempStr := source[:]
	desc := make([]byte, len(tempStr)*2)
	for i := 0; i < len(tempStr); i++ {
		flag := false
		var escape byte
		switch tempStr[i] {
		case '\r':
			flag = true
			escape = '\r'
			break
		case '\n':
			flag = true
			escape = '\n'
			break
		case '\\':
			flag = true
			escape = '\\'
			break
		case '\'':
			flag = true
			escape = '\''
			break
		case '"':
			flag = true
			escape = '"'
			break
		case '\032':
			flag = true
			escape = 'Z'
			break
		default:
		}
		if flag {
			desc[j] = '\\'
			desc[j+1] = escape
			j = j + 2
		} else {
			desc[j] = tempStr[i]
			j = j + 1
		}
	}
	return string(desc[0:j])
}

//makeTimelineSQLString makes a SQL query string to provide the timeline to left join
//on time-based series reports to provide all datapoints
func makeTimelineSQLString(startTime time.Time, endTime time.Time, intervalSec int64) (string, error) {
	divisor := strconv.FormatInt(intervalSec*1000, 10)

	sqlStr := "SELECT DISTINCT (("
	sqlStr += "(" + dateFormat(startTime) + "/" + divisor + ")"
	sqlStr += "+a*10000+b*1000+c*100+d*10+e" + ")*" + divisor + ") AS time_trunc FROM"
	sqlStr += " (" + makeSeqSQLString("a", 9) + "), "
	sqlStr += " (" + makeSeqSQLString("b", 10) + "), "
	sqlStr += " (" + makeSeqSQLString("c", 10) + "), "
	sqlStr += " (" + makeSeqSQLString("d", 10) + "), "
	sqlStr += " (" + makeSeqSQLString("e", 10) + ") "
	sqlStr += "WHERE time_trunc < " + dateFormat(endTime)
	return sqlStr, nil
}

//makeSeriesSQLString makes a SQL string to get the sequence 0 to max-1
//example: maxSeriesSQLString("a",5)
//SELECT 0 as a UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
// 0, 1, 2, 3, 4
func makeSeqSQLString(columnName string, max int) string {
	if max < 0 {
		return ""
	}
	sqlStr := fmt.Sprintf("SELECT 0 as %s", columnName)
	for i := 1; i < max; i++ {
		sqlStr += fmt.Sprintf(" UNION SELECT %d", i)
	}
	return sqlStr
}

//dateFormat returns the proper sql string for the corresponding time
func dateFormat(t time.Time) string {
	//return t.Format(time.RFC3339)
	return strconv.FormatInt(t.UnixNano()/1e6, 10)
}

// getMapValue gets the value for the row for CATEGORIES reports
func getMapValue(m map[string]interface{}) string {
	// We don't care about the value
	// Delete it so we can find the value of the remaining entry
	delete(m, "value")
	// Get the value for the only remaining entry and return it
	for _, v := range m {
		str := fmt.Sprintf("%v", v)
		return str
	}
	return ""
}

// getDistinctValues returns the distinct values to be used
// in a CATEGORIES_SERIES report
func getDistinctValues(reportEntry *ReportEntry, startTime time.Time, endTime time.Time) ([]string, error) {
	categoriesSQLStr, err := makeCategoriesSQLString(reportEntry, startTime, endTime)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(categoriesSQLStr)
	if err != nil {
		return nil, err
	}
	categories, err := getRows(rows, reportEntry.QueryCategories.CategoriesLimit)
	if err != nil {
		return nil, err
	}

	var values []string

	for _, v := range categories {
		str := getMapValue(v)
		if str != "" {
			values = append(values, str)
		}
	}

	return values, nil
}