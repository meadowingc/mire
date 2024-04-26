package user_preferences

import (
	"log"
	"reflect"
	"strconv"

	"codeberg.org/meadowingc/mire/sqlite"
)

type UserPreferences struct {
	NumPostsToShowInHomeScreen       int `db:"numPostsToShowInHomeScreen" default:"300"`
	NumUnreadPostsToShowInHomeScreen int `db:"numUnreadPostsToShowInHomeScreen" default:"7"`
}

func SetFieldValue(field reflect.Value, value string) {
	switch field.Kind() {
	case reflect.Int:
		intVal, err := strconv.Atoi(value)
		if err != nil {
			log.Fatalf("SetFieldValue:: Error converting preference value to int: %v", err)
		}
		field.SetInt(int64(intVal))
	case reflect.String:
		field.SetString(value)
	case reflect.Bool:
		boolVal, err := strconv.ParseBool(value)
		if err != nil {
			log.Fatalf("SetFieldValue:: Error converting preference value to bool: %v", err)
		}
		field.SetBool(boolVal)
	default:
		log.Fatalf("SetFieldValue:: Unsupported field type: %v", field.Kind())
	}
}

func GetDefaultUserPreferences() *UserPreferences {
	userPreferences := UserPreferences{}
	valPointer := reflect.ValueOf(&userPreferences)
	val := valPointer.Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("db")
		if tag == "" {
			log.Fatalf("GetUserPreferences:: Field %s does not have a 'db' tag", field.Name)
		}

		defaultValue := field.Tag.Get("default")

		// set the field value taking into account it's type. Also set the
		// default value if the preference is not found
		SetFieldValue(val.Field(i), defaultValue)
	}

	return &userPreferences
}

func GetUserPreferences(db *sqlite.DB, userId int) *UserPreferences {
	userPreferences := UserPreferences{}
	valPointer := reflect.ValueOf(&userPreferences)
	val := valPointer.Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("db")
		if tag == "" {
			log.Fatalf("GetUserPreferences:: Field %s does not have a 'db' tag", field.Name)
		}

		preferenceValue := db.GetSingleUserPreference(userId, tag)
		if preferenceValue == nil {
			// Preference not found for this user
			// Set default value
			defaultValue := field.Tag.Get("default")
			if defaultValue == "" {
				log.Fatalf("GetUserPreferences:: Field %s does not have a 'default' tag", field.Name)
			}
			preferenceValue = &defaultValue
		}

		// set the field value taking into account it's type. Also set the
		// default value if the preference is not found
		SetFieldValue(val.Field(i), *preferenceValue)
	}

	return &userPreferences
}

func SaveUserPreferences(db *sqlite.DB, userID int, userPreferences *UserPreferences) {
	val := reflect.ValueOf(userPreferences).Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := field.Type()
		fieldName := typ.Field(i).Name
		dbTag := typ.Field(i).Tag.Get("db")

		// Convert the field value to a string
		var fieldValue string
		switch fieldType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fieldValue = strconv.FormatInt(field.Int(), 10)
		case reflect.Bool:
			fieldValue = strconv.FormatBool(field.Bool())
		default:
			log.Fatalf("SaveUserPreferences:: Unsupported type for field %s", fieldName)
		}

		err := db.SaveSingleUserPreference(userID, dbTag, fieldValue)
		if err != nil {
			log.Fatalf(
				"SaveUserPreferences:: Error saving user preference %s: %v",
				fieldName,
				err,
			)
		}
	}
}
