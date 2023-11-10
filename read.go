// Copyright 2022 exl Author. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//      http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exl

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/tealeg/xlsx/v3"
)

type (
	ReadConfigurator interface{ ReadConfigure(rc *ReadConfig) }
	ReadConfig       struct {
		// The tag name to use when looking for fields in the target struct.
		// Defaults to "excel".
		TagName string
		// The index of the worksheet to be read.
		// Defaults to 0, the first worksheet.
		SheetIndex int
		// The row index at which the column headers are read from.
		// Zero-based, defaults to 0.
		HeaderRowIndex int
		// Start the data reading at this row.
		// The header row counts as row.
		// Zero-based, defaults to 1.
		DataStartRowIndex int
		// Configure the default string unmarshaler to trim space after reading a cell.
		// Does not impact any other default unmarshaler,
		// but is available to custom unmarshalers via ExcelUnmarshalParameters.TrimSpace.
		// Defaults to false.
		TrimSpace bool
		// Fallback date formats for date parsing.
		// If an Excel cell is to be unmarshalled into a date,
		// and that cell is either not formatted as Date or contains raw text
		// (which can happen if Excel does not correctly recognize the date format)
		// then these formats are used in the order specified to try and parse
		// the raw cell value into a date.
		// There are no fallback formats configured by default.
		FallbackDateFormats []string
		// Skip reading columns for which no target field is found.
		// Defaults to true.
		SkipUnknownColumns bool
		// Skip reading columns, if there is a target field,
		// but the target type is unsupported
		// or caused an error when determining the unmarshaler to use.
		// Defaults to false.
		SkipUnknownTypes bool
		// Configure how errors during unmarshaling are handled.
		// Unmarshaling errors are e.g. invalid number formats in the cell,
		// date parsing with invalid input,
		// or attempting to unmarshal non-numeric text into a numeric field.
		// Defaults to UnmarshalErrorAbort.
		UnmarshalErrorHandling UnmarshalErrorHandling
		// If UnmarshalErrorHandling is configured as UnmarshalErrorCollect,
		// this option limits the number of errors which are collected before
		// parsing is aborted.
		// Configure a limit of 0 to collect all errors, without upper limit.
		// Defaults to 10.
		MaxUnmarshalErrors uint64
	}
	UnmarshalErrorHandling uint8
	FieldError             struct {
		RowIndex     int // 0-based row index. Printed as 1-based row number in error text.
		ColumnIndex  int // 0-based column index.
		ColumnHeader string
		Err          error
	}
	ContentError struct {
		FieldErrors  []FieldError
		LimitReached bool
	}
)

var (
	// Ensure FieldError implements the error interface
	_ error = FieldError{}
	// Ensure FieldError can be unwrapped
	_ interface {
		Unwrap() error
	} = FieldError{}
	// Ensure ContentError implements the error interface
	_ error = ContentError{}
)

// Error implements error.
func (e FieldError) Error() string {
	return fmt.Sprintf("error unmarshaling column \"%s\" in row %d: %s", e.ColumnHeader, e.RowIndex+1, e.Err.Error())
}

// Error implements the anonymous unwrap interface used by errors.Unwrap and others.
func (e FieldError) Unwrap() error {
	return e.Err
}

// Error implements error.
func (e ContentError) Error() string {
	if e.LimitReached {
		return fmt.Sprintf("too many (%d) errors reading data from Excel", len(e.FieldErrors))
	} else {
		return fmt.Sprintf("%d errors reading data from Excel", len(e.FieldErrors))
	}
}

// Error implements the anonymous unwrap interface used by errors.Unwrap and others.
func (e ContentError) Unwrap() []error {
	// Slice needs to be type-adjusted
	errs := make([]error, len(e.FieldErrors))
	for i, v := range e.FieldErrors {
		errs[i] = v
	}
	return errs
}

const (
	// Ignore any errors during unmarshaling
	UnmarshalErrorIgnore UnmarshalErrorHandling = iota
	// Abort reading when encountering the first unmarshaling error
	UnmarshalErrorAbort
	// Collect unmarshaling errors up to a limit, but continue reading.
	// Collected errors are returned as one error at the end, of type
	UnmarshalErrorCollect
)

var (
	defaultReadConfig = func() *ReadConfig {
		return &ReadConfig{
			TagName:                "excel",
			DataStartRowIndex:      1,
			SkipUnknownColumns:     true,
			UnmarshalErrorHandling: UnmarshalErrorAbort,
			MaxUnmarshalErrors:     10,
		}
	}
	ErrSheetIndexOutOfRange        = errors.New("exl: sheet index out of range")
	ErrHeaderRowIndexOutOfRange    = errors.New("exl: header row index out of range")
	ErrDataStartRowIndexOutOfRange = errors.New("exl: data start row index out of range")
	ErrNoUnmarshaler               = errors.New("no unmarshaler")
	ErrNoDestinationField          = errors.New("no destination field with matching tag")
)

func readStrings(maxCol int, row *xlsx.Row) []string {
	ls := make([]string, maxCol)
	for i := 0; i < maxCol; i++ {
		ls[i] = row.GetCell(i).Value
	}
	return ls
}

func GetUnmarshalFunc(destField reflect.Value) UnmarshalExcelFunc {
	if destField.CanInterface() {

		intf := getFieldInterface(destField)
		if intf != nil {

			// Prefer ExcelUnmarshaler, if implemented
			if _, ok := intf.(ExcelUnmarshaler); ok {
				return UnmarshalExcelUnmarshaler
			}

			// Then handle specific types with special implementation
			if destField.Type() == reflect.TypeOf(time.Time{}) {
				return UnmarshalTime
			}

			// Then utilize TextUnmarshaler, e.g. for things like decimal.Decimal
			if _, ok := intf.(encoding.TextUnmarshaler); ok {
				return UnmarshalTextUnmarshaler
			}

		}
	}

	// And for primitive types, use custom unmarshaling funcs
	kind := destField.Type().Kind()
	if kind == reflect.Ptr {
		kind = destField.Type().Elem().Kind()
	}
	unmarshalFunc, ok := DefaultUnmarshalFuncs[kind]
	if ok {
		return unmarshalFunc
	}

	return nil
}

// Read io.Reader each row bind to `T`
func Read[T ReadConfigurator](reader io.Reader, filterFunc ...func(t T) (add bool)) ([]T, error) {
	if bytes, err := io.ReadAll(reader); err != nil {
		return []T(nil), err
	} else {
		return ReadBinary(bytes, filterFunc...)
	}
}

// ReadFile each row bind to `T`
func ReadFile[T ReadConfigurator](file string, filterFunc ...func(t T) (add bool)) ([]T, error) {
	if bytes, err := os.ReadFile(file); err != nil {
		return []T(nil), err
	} else {
		return ReadBinary(bytes, filterFunc...)
	}
}

// ReadBinary each row bind to `T`
func ReadBinary[T ReadConfigurator](bytes []byte, filterFunc ...func(t T) (add bool)) ([]T, error) {
	f, err := xlsx.OpenBinary(bytes)
	if err != nil {
		return nil, err
	}
	var t T
	rc := defaultReadConfig()
	t.ReadConfigure(rc)
	if rc.SheetIndex < 0 || rc.SheetIndex > len(f.Sheet)-1 {
		return nil, ErrSheetIndexOutOfRange
	}
	sheet := f.Sheets[rc.SheetIndex]
	if rc.HeaderRowIndex < 0 || rc.HeaderRowIndex > sheet.MaxRow-1 {
		return nil, ErrHeaderRowIndexOutOfRange
	}
	if rc.DataStartRowIndex < 0 || rc.DataStartRowIndex > sheet.MaxRow-1 {
		return nil, ErrDataStartRowIndexOutOfRange
	}
	headerRow, _ := sheet.Row(rc.HeaderRowIndex)
	maxCol := sheet.MaxCol
	headers := readStrings(maxCol, headerRow)
	type fieldInfo struct {
		reflectFieldIndex int
		header            string
		unmarshalFunc     UnmarshalExcelFunc
	}
	// Key: Header / Tag name
	// Value: Reflection field index
	tagToFieldMap := make(map[string]int, 0)
	// Key: Column Index
	// Value: Unmarshaling Info
	columnFields := make([]fieldInfo, len(headers))

	typ := reflect.TypeOf(t).Elem()
	for i := 0; i < typ.NumField(); i++ {
		if ta := typ.Field(i).Tag; ta != "" {
			if tt, have := ta.Lookup(rc.TagName); have {
				tagToFieldMap[tt] = i
			}
		}
	}

	{
		val := reflect.New(typ).Elem()

		for columnIndex, header := range headers {
			reflectFieldIndex, have := tagToFieldMap[header]
			if !have {
				if rc.SkipUnknownColumns {
					// Skip reading this field
					columnFields[columnIndex] = fieldInfo{
						reflectFieldIndex: reflectFieldIndex,
						header:            header,
						unmarshalFunc:     nil,
					}
					continue
				} else {
					return nil, fmt.Errorf("%w for column \"%s\" at index %d", ErrNoDestinationField, header, columnIndex)
				}
			}

			field := val.Field(reflectFieldIndex)

			unmarshaler := GetUnmarshalFunc(field)
			if unmarshaler == nil {
				if rc.SkipUnknownTypes {
					// Skip reading this field
					columnFields[columnIndex] = fieldInfo{
						reflectFieldIndex: reflectFieldIndex,
						header:            header,
						unmarshalFunc:     nil,
					}
					continue
				} else {
					return nil, fmt.Errorf("%w for column \"%s\" at index %d", ErrNoUnmarshaler, header, columnIndex)
				}
			}

			columnFields[columnIndex] = fieldInfo{
				reflectFieldIndex: reflectFieldIndex,
				header:            header,
				unmarshalFunc:     unmarshaler,
			}
		}
	}

	unmarshalConfig := &ExcelUnmarshalParameters{
		TrimSpace:           rc.TrimSpace,
		Date1904:            f.Date1904,
		FallbackDateFormats: rc.FallbackDateFormats,
	}

	collectedErrors := make([]FieldError, 0)

	ts := make([]T, 0)
	for rowIndex := 0; rowIndex < sheet.MaxRow; rowIndex++ {
		if rowIndex >= rc.DataStartRowIndex {
			val := reflect.New(typ).Elem()
			if row, _ := sheet.Row(rowIndex); row != nil {

				for columnIndex, fi := range columnFields {
					// If there is no unmarshal function,
					// this field has been skipped by previous logic.
					// e.g. no destination field, or unknown type.
					if fi.unmarshalFunc == nil {
						continue
					}
					cell := row.GetCell(columnIndex)

					destField := val.Field(fi.reflectFieldIndex)
					err = fi.unmarshalFunc(destField, cell, unmarshalConfig)
					if err != nil && rc.UnmarshalErrorHandling != UnmarshalErrorIgnore {
						ferr := FieldError{
							RowIndex:     rowIndex,
							ColumnIndex:  columnIndex,
							ColumnHeader: fi.header,
							Err:          err,
						}
						if rc.UnmarshalErrorHandling == UnmarshalErrorAbort {
							return nil, ferr
						} else {
							collectedErrors = append(collectedErrors, ferr)
							if rc.MaxUnmarshalErrors > 0 && uint64(len(collectedErrors)) >= rc.MaxUnmarshalErrors {
								return nil, ContentError{
									FieldErrors:  collectedErrors,
									LimitReached: true,
								}
							}
						}
					}
				}
				nT := val.Addr().Interface().(T)
				add := true
				if filterFunc != nil && len(filterFunc) > 0 {
					for _, fF := range filterFunc {
						if fF != nil {
							add = fF(nT)
							if !add {
								break
							}
						}
					}
				}
				if add {
					ts = append(ts, nT)
				}
			}
		}
	}
	if len(collectedErrors) > 0 {
		return nil, ContentError{
			FieldErrors:  collectedErrors,
			LimitReached: false,
		}
	}
	return ts, nil
}

// ReadExcel walk func from excel
func ReadExcel(file string, sheetIndex int, walk func(index int, rows *xlsx.Row)) error {
	f, err := xlsx.OpenFile(file)
	if err != nil {
		return err
	}
	sheet := f.Sheets[sheetIndex]
	for i := 0; i < sheet.MaxRow; i++ {
		if row, _ := sheet.Row(i); row != nil {
			walk(i, row)
		}
	}
	return nil
}
