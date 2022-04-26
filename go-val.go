package quickjs

import (
	"reflect"
	"fmt"
	// "strings"
)

func setValue(dest reflect.Value, val interface{}) error {
	dt := dest.Type()
	if val == nil {
		if dest.CanAddr() {
			dest.Set(reflect.Zero(dt))
		}
		return nil
	}
	if dt.Kind() == reflect.Ptr {
		et := dt.Elem()
		ev := makeValue(et)
		setValue(ev, val)
		dest.Set(ev.Addr())
		return nil
	}
	v := reflect.ValueOf(val)
	vt := reflect.TypeOf(val)
	if vt.AssignableTo(dt) {
		dest.Set(v)
		return nil
	}

	if vt.ConvertibleTo(dt) {
		dest.Set(v.Convert(dt))
		return nil
	}

	switch v.Kind() {
	case reflect.Map:
		switch dest.Kind() {
		case reflect.Struct:
			return map2Struct(dest, v)
		case reflect.Map:
			md := reflect.MakeMap(dt)
			if err := map2Map(md, v); err != nil {
				return err
			}
			dest.Set(md)
			return nil
		default:
		}
	case reflect.Slice:
		if dest.Kind() == reflect.Slice {
			return copySlice(dest, v)
		}
	}

	return fmt.Errorf("cannot convert %s to %s", vt, dt)
}

func map2Struct(dest reflect.Value, v reflect.Value) error {
	dt := dest.Type()
	for i:=0; i<dt.NumField(); i++ {
		ft := dt.Field(i)
		fv := dest.Field(i)
		fn := ft.Name
		tag := ft.Tag
		if tv := tag.Get("json"); len(tv) > 0 {
			fn = tv
		} else {
			fn = lowerFirst(fn)
		}
		mv := v.MapIndex(reflect.ValueOf(fn))
		if mv.IsValid() {
			if err := setValue(fv, mv.Interface()); err != nil {
				return err
			}
		}
	}

	return nil
}

func map2Map(dest reflect.Value, v reflect.Value) error {
	dt := dest.Type()
	kt := dt.Key()
	et := dt.Elem()

	it := v.MapRange()
	for it.Next() {
		vk := it.Key()
		dk := makeValue(kt)
		if err := setValue(dk, vk.Interface()); err != nil {
			return err
		}

		vv := it.Value()
		dv := makeValue(et)
		if err := setValue(dv, vv.Interface()); err != nil {
			return err
		}

		dest.SetMapIndex(dk, dv)
	}

	return nil
}

func copySlice(dest reflect.Value, v reflect.Value) error {
	l := v.Len()
	if l == 0 {
		dest.SetLen(0)
		return nil
	}
	newDest := reflect.MakeSlice(dest.Type(), l, l)
	for i:=0; i<l; i++ {
		if err := setValue(newDest.Index(i), v.Index(i).Interface()); err != nil {
			return err
		}
	}
	dest.Set(newDest)
	return nil
}

func makeValue(t reflect.Type) reflect.Value {
	switch t.Kind() {
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return reflect.Indirect(reflect.New(reflect.TypeOf("")))
		}
		return makeSlice(t.Elem())
	case reflect.Array:
		return makeArray(t.Elem(), t.Len())
	case reflect.Bool,reflect.Int,reflect.Uint,
			reflect.Int8,reflect.Int16,reflect.Int32,reflect.Int64,
			reflect.Uint8,reflect.Uint16,reflect.Uint32,reflect.Uint64,
			reflect.Float32,reflect.Float64,reflect.String,
			/*reflect.Array,*/reflect.Map,reflect.Struct,
			reflect.Interface/*,reflect.Ptr*/,reflect.Func:
		return reflect.Indirect(reflect.New(t))
	/*
	case reflect.Map:
		return reflect.MakeMap(t)
	*/
	case reflect.Ptr:
		el := makeValue(t.Elem())
		ptr := reflect.Indirect(reflect.New(t))
		ptr.Set(el.Addr())
		return ptr
	default:
		panic("unsupport type")
	}
}

func makeArray(el reflect.Type, l int) reflect.Value {
	t := reflect.ArrayOf(l, el)
	return reflect.Indirect(reflect.New(t))
}

func makeSlice(el reflect.Type) reflect.Value {
	t := reflect.SliceOf(el)
	return reflect.Indirect(reflect.New(t))
}
