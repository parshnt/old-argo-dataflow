#!/bin/sh

echo '// Code generated by gen.sh. DO NOT EDIT.' > meta.go
sed 's/package v1alpha1/package golang/' < ../../api/v1alpha1/meta.go >> meta.go