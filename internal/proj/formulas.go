package proj

import (
	"fmt"
	"math"
)

// formula is a projection's math in terms of (delta-longitude, latitude)
// in radians, with latitude in the formula's own convention.
type formula interface {
	fwd(dlam, phi float64) (x, y float64, ok bool)
	inv(x, y float64) (dlam, phi float64, ok bool)
}

// Equirectangular / simple cylindrical (spherical, as in ISIS).
type eqcF struct{ R, cts, phi0 float64 }

func (f eqcF) fwd(dl, phi float64) (float64, float64, bool) {
	return f.R * f.cts * dl, f.R * (phi - f.phi0), true
}
func (f eqcF) inv(x, y float64) (float64, float64, bool) {
	phi := y/f.R + f.phi0
	if math.Abs(phi) > math.Pi/2+1e-12 {
		return 0, 0, false
	}
	return x / (f.R * f.cts), clampLat(phi), true
}

// Geographic: x = lon degrees, y = lat degrees.
type geoF struct{ lon0deg float64 }

func (f geoF) fwd(dl, phi float64) (float64, float64, bool) {
	return f.lon0deg + dl*R2D, phi * R2D, true
}
func (f geoF) inv(x, y float64) (float64, float64, bool) {
	if math.Abs(y) > 90+1e-9 {
		return 0, 0, false
	}
	return (x - f.lon0deg) * D2R, clampLat(y * D2R), true
}

// Sinusoidal (spherical, as in ISIS).
type sinuF struct{ R float64 }

func (f sinuF) fwd(dl, phi float64) (float64, float64, bool) {
	return f.R * dl * math.Cos(phi), f.R * phi, true
}
func (f sinuF) inv(x, y float64) (float64, float64, bool) {
	phi := y / f.R
	if math.Abs(phi) > math.Pi/2+1e-12 {
		return 0, 0, false
	}
	c := math.Cos(phi)
	if c < 1e-12 {
		if math.Abs(x) > 1e-6*f.R {
			return 0, 0, false
		}
		return 0, clampLat(phi), true
	}
	dl := x / (f.R * c)
	if math.Abs(dl) > math.Pi+1e-9 {
		return 0, 0, false
	}
	return dl, phi, true
}

// Orthographic (spherical, as in ISIS).
type orthoF struct{ R, sp0, cp0 float64 }

func (f orthoF) fwd(dl, phi float64) (float64, float64, bool) {
	sp, cp := math.Sincos(phi)
	sl, cl := math.Sincos(dl)
	g := f.sp0*sp + f.cp0*cp*cl
	if g < -1e-10 {
		return 0, 0, false
	}
	return f.R * cp * sl, f.R * (f.cp0*sp - f.sp0*cp*cl), true
}
func (f orthoF) inv(x, y float64) (float64, float64, bool) {
	rho := math.Hypot(x, y)
	if rho > f.R*(1+1e-12) {
		return 0, 0, false
	}
	if rho < 1e-12*f.R {
		return 0, math.Atan2(f.sp0, f.cp0), true
	}
	sc := math.Min(rho/f.R, 1)
	c := math.Asin(sc)
	cc := math.Cos(c)
	phi := math.Asin(clamp1(cc*f.sp0 + y*sc*f.cp0/rho))
	dl := math.Atan2(x*sc, rho*cc*f.cp0-y*sc*f.sp0)
	return dl, phi, true
}

// helpers shared by conformal projections (Snyder)
func tsfn(phi, e float64) float64 {
	sp := math.Sin(phi)
	t := math.Tan(0.5 * (math.Pi/2 - phi))
	if e == 0 {
		return t
	}
	return t / math.Pow((1-e*sp)/(1+e*sp), 0.5*e)
}

func msfn(phi, e float64) float64 {
	sp, cp := math.Sincos(phi)
	return cp / math.Sqrt(1-e*e*sp*sp)
}

func phi2(t, e float64) (float64, bool) {
	phi := math.Pi/2 - 2*math.Atan(t)
	if e == 0 {
		return phi, true
	}
	for i := 0; i < 30; i++ {
		sp := e * math.Sin(phi)
		d := math.Pi/2 - 2*math.Atan(t*math.Pow((1-sp)/(1+sp), 0.5*e)) - phi
		phi += d
		if math.Abs(d) < 1e-14 {
			return phi, true
		}
	}
	return phi, true
}

// Polar stereographic (ellipsoidal, Snyder / ISIS).
type psF struct {
	a, e, k0, s float64
	atPole      bool
	mc, tc, e4  float64
}

func newPS(a, e, clatGraphic, k0 float64) psF {
	f := psF{a: a, e: e, k0: k0, s: 1}
	if clatGraphic < 0 {
		f.s = -1
	}
	f.e4 = math.Sqrt(math.Pow(1+e, 1+e) * math.Pow(1-e, 1-e))
	phic := f.s * clatGraphic
	if math.Pi/2-phic > 1e-10 {
		f.mc = msfn(phic, e)
		f.tc = tsfn(phic, e)
		if math.Abs(f.tc) < 1e-15 {
			f.atPole = true
		}
	} else {
		f.atPole = true
	}
	return f
}

func (f psF) fwd(dl, phi float64) (float64, float64, bool) {
	lp := f.s * dl
	pp := f.s * phi
	if pp <= -math.Pi/2+1e-10 {
		return 0, 0, false
	}
	t := tsfn(pp, f.e)
	var rho float64
	if f.atPole {
		rho = 2 * f.a * f.k0 * t / f.e4
	} else {
		rho = f.a * f.mc * t / f.tc
	}
	sl, cl := math.Sincos(lp)
	return f.s * rho * sl, -f.s * rho * cl, true
}

func (f psF) inv(x, y float64) (float64, float64, bool) {
	rho := math.Hypot(x, y)
	var t float64
	if f.atPole {
		t = rho * f.e4 / (2 * f.a * f.k0)
	} else {
		t = rho * f.tc / (f.a * f.mc)
	}
	pp, ok := phi2(t, f.e)
	if !ok {
		return 0, 0, false
	}
	lp := 0.0
	if rho > 0 {
		lp = math.Atan2(f.s*x, -f.s*y)
	}
	return f.s * lp, f.s * pp, true
}

// Mercator (ellipsoidal).
type mercF struct{ a, e, k0 float64 }

func (f mercF) fwd(dl, phi float64) (float64, float64, bool) {
	if math.Abs(phi) >= math.Pi/2-1e-10 {
		return 0, 0, false
	}
	return f.a * f.k0 * dl, -f.a * f.k0 * math.Log(tsfn(phi, f.e)), true
}
func (f mercF) inv(x, y float64) (float64, float64, bool) {
	phi, ok := phi2(math.Exp(-y/(f.a*f.k0)), f.e)
	return x / (f.a * f.k0), phi, ok
}

// Lambert conformal conic, 2SP (ellipsoidal).
type lccF struct{ a, e, n, F, rho0 float64 }

func newLCC(a, e, p1, p2, p0 float64) (lccF, error) {
	f := lccF{a: a, e: e}
	if math.Abs(p1+p2) < 1e-10 {
		return f, fmt.Errorf("lambertconformal: standard parallels must not be symmetric about the equator")
	}
	m1, t1 := msfn(p1, e), tsfn(p1, e)
	if math.Abs(p1-p2) > 1e-10 {
		m2, t2 := msfn(p2, e), tsfn(p2, e)
		f.n = (math.Log(m1) - math.Log(m2)) / (math.Log(t1) - math.Log(t2))
	} else {
		f.n = math.Sin(p1)
	}
	f.F = m1 / (f.n * math.Pow(t1, f.n))
	f.rho0 = f.rhoOf(p0)
	return f, nil
}

func (f lccF) rhoOf(phi float64) float64 {
	if math.Abs(math.Abs(phi)-math.Pi/2) < 1e-12 {
		if phi*f.n > 0 {
			return 0
		}
		return math.Inf(1)
	}
	return f.a * f.F * math.Pow(tsfn(phi, f.e), f.n)
}

func (f lccF) fwd(dl, phi float64) (float64, float64, bool) {
	rho := f.rhoOf(phi)
	if math.IsInf(rho, 0) {
		return 0, 0, false
	}
	th := f.n * dl
	st, ct := math.Sincos(th)
	return rho * st, f.rho0 - rho*ct, true
}

func (f lccF) inv(x, y float64) (float64, float64, bool) {
	sn := 1.0
	if f.n < 0 {
		sn = -1
	}
	dy := f.rho0 - y
	rho := sn * math.Hypot(x, dy)
	if rho == 0 {
		return 0, sn * math.Pi / 2, true
	}
	t := math.Pow(rho/(f.a*f.F), 1/f.n)
	th := math.Atan2(sn*x, sn*dy)
	phi, ok := phi2(t, f.e)
	return th / f.n, phi, ok
}

// Lambert azimuthal equal area (ellipsoidal, Snyder).
type laeaF struct {
	a, e, e2, qp, rq, d float64
	sb1, cb1, phi1      float64
	mode                int // 0 oblique, 1 north polar, -1 south polar
}

func qfn(sp, e float64) float64 {
	if e == 0 {
		return 2 * sp
	}
	e2 := e * e
	return (1 - e2) * (sp/(1-e2*sp*sp) - 1/(2*e)*math.Log((1-e*sp)/(1+e*sp)))
}

func newLAEA(a, e, phi1 float64) laeaF {
	f := laeaF{a: a, e: e, e2: e * e, phi1: phi1}
	f.qp = qfn(1, e)
	f.rq = a * math.Sqrt(f.qp/2)
	switch {
	case math.Abs(phi1-math.Pi/2) < 1e-10:
		f.mode = 1
	case math.Abs(phi1+math.Pi/2) < 1e-10:
		f.mode = -1
	default:
		b1 := math.Asin(clamp1(qfn(math.Sin(phi1), e) / f.qp))
		f.sb1, f.cb1 = math.Sincos(b1)
		f.d = a * msfn(phi1, e) / (f.rq * f.cb1)
	}
	return f
}

func (f laeaF) fwd(dl, phi float64) (float64, float64, bool) {
	q := qfn(math.Sin(phi), f.e)
	sl, cl := math.Sincos(dl)
	switch f.mode {
	case 1:
		rho := f.a * math.Sqrt(math.Max(f.qp-q, 0))
		return rho * sl, -rho * cl, true
	case -1:
		rho := f.a * math.Sqrt(math.Max(f.qp+q, 0))
		return rho * sl, rho * cl, true
	}
	sb := clamp1(q / f.qp)
	cb := math.Sqrt(1 - sb*sb)
	den := 1 + f.sb1*sb + f.cb1*cb*cl
	if den < 1e-12 {
		return 0, 0, false
	}
	B := f.rq * math.Sqrt(2/den)
	return B * f.d * cb * sl, (B / f.d) * (f.cb1*sb - f.sb1*cb*cl), true
}

func (f laeaF) inv(x, y float64) (float64, float64, bool) {
	var q, dl float64
	switch f.mode {
	case 1, -1:
		rho := math.Hypot(x, y)
		r := (rho / f.a) * (rho / f.a)
		if r > f.qp*2*(1+1e-12) {
			return 0, 0, false
		}
		if f.mode == 1 {
			q = f.qp - r
			dl = math.Atan2(x, -y)
		} else {
			q = r - f.qp
			dl = math.Atan2(x, y)
		}
	default:
		xd, yd := x/f.d, y*f.d
		rho := math.Hypot(xd, yd)
		if rho < 1e-12*f.a {
			return 0, f.phi1, true
		}
		s := rho / (2 * f.rq)
		if s > 1+1e-12 {
			return 0, 0, false
		}
		ce := 2 * math.Asin(math.Min(s, 1))
		sce, cce := math.Sincos(ce)
		q = f.qp * (cce*f.sb1 + f.d*y*sce*f.cb1/rho)
		dl = math.Atan2(x*sce, f.d*rho*f.cb1*cce-f.d*f.d*y*f.sb1*sce)
	}
	return dl, authalicInv(q, f.e, f.qp), true
}

func authalicInv(q, e, qp float64) float64 {
	if math.Abs(math.Abs(q)-qp) < 1e-12 {
		return math.Copysign(math.Pi/2, q)
	}
	phi := math.Asin(clamp1(q / 2))
	if e == 0 {
		return phi
	}
	e2 := e * e
	for i := 0; i < 30; i++ {
		sp, cp := math.Sincos(phi)
		c := 1 - e2*sp*sp
		d := c * c / (2 * cp) * (q/(1-e2) - sp/c + 1/(2*e)*math.Log((1-e*sp)/(1+e*sp)))
		phi += d
		if math.Abs(d) < 1e-14 {
			break
		}
	}
	return phi
}

// Transverse Mercator, Krüger series to n^6 (exact on the sphere).
type tmF struct {
	e, k0A, y0 float64
	alp, bet   [7]float64
}

func newTM(a, b, phi0, k0 float64) tmF {
	f := tmF{}
	fl := (a - b) / a
	f.e = math.Sqrt(fl * (2 - fl))
	n := fl / (2 - fl)
	n2 := n * n
	A := a / (1 + n) * (1 + n2/4 + n2*n2/64 + n2*n2*n2/256)
	f.k0A = k0 * A
	n3, n4, n5, n6 := n2*n, n2*n2, n2*n2*n, n2*n2*n2
	f.alp = [7]float64{0,
		n/2 - 2*n2/3 + 5*n3/16 + 41*n4/180 - 127*n5/288 + 7891*n6/37800,
		13*n2/48 - 3*n3/5 + 557*n4/1440 + 281*n5/630 - 1983433*n6/1935360,
		61*n3/240 - 103*n4/140 + 15061*n5/26880 + 167603*n6/181440,
		49561*n4/161280 - 179*n5/168 + 6601661*n6/7257600,
		34729*n5/80640 - 3418889*n6/1995840,
		212378941 * n6 / 319334400,
	}
	f.bet = [7]float64{0,
		n/2 - 2*n2/3 + 37*n3/96 - n4/360 - 81*n5/512 + 96199*n6/604800,
		n2/48 + n3/15 - 437*n4/1440 + 46*n5/105 - 1118711*n6/3870720,
		17*n3/480 - 37*n4/840 - 209*n5/4480 + 5569*n6/90720,
		4397*n4/161280 - 11*n5/504 - 830251*n6/7257600,
		4583*n5/161280 - 108847*n6/3991680,
		20648693 * n6 / 638668800,
	}
	_, y0, _ := f.fwd(0, phi0)
	f.y0 = y0
	return f
}

func (f tmF) fwd(dl, phi float64) (float64, float64, bool) {
	if math.Abs(dl) > math.Pi/2 {
		return 0, 0, false
	}
	sp := math.Sin(phi)
	t := math.Sinh(math.Atanh(sp) - f.e*math.Atanh(f.e*sp))
	sl, cl := math.Sincos(dl)
	xip := math.Atan2(t, cl)
	s := sl / math.Sqrt(1+t*t)
	if math.Abs(s) >= 1-1e-12 {
		return 0, 0, false
	}
	etap := math.Atanh(s)
	xi, eta := xip, etap
	for j := 1; j <= 6; j++ {
		s2, c2 := math.Sincos(2 * float64(j) * xip)
		xi += f.alp[j] * s2 * math.Cosh(2*float64(j)*etap)
		eta += f.alp[j] * c2 * math.Sinh(2*float64(j)*etap)
	}
	return f.k0A * eta, f.k0A*xi - f.y0, true
}

func (f tmF) inv(x, y float64) (float64, float64, bool) {
	xi := (y + f.y0) / f.k0A
	eta := x / f.k0A
	xip, etap := xi, eta
	for j := 1; j <= 6; j++ {
		s2, c2 := math.Sincos(2 * float64(j) * xi)
		xip -= f.bet[j] * s2 * math.Cosh(2*float64(j)*eta)
		etap -= f.bet[j] * c2 * math.Sinh(2*float64(j)*eta)
	}
	sxi, cxi := math.Sincos(xip)
	she := math.Sinh(etap)
	tp := sxi / math.Hypot(she, cxi) // tan(conformal lat)
	dl := math.Atan2(she, cxi)
	// conformal -> geodetic (Karney's Newton iteration)
	e := f.e
	tau := tp
	if e > 0 {
		e2m := 1 - e*e
		for i := 0; i < 10; i++ {
			sig := math.Sinh(e * math.Atanh(e*tau/math.Sqrt(1+tau*tau)))
			tpi := tau*math.Sqrt(1+sig*sig) - sig*math.Sqrt(1+tau*tau)
			d := (tp - tpi) / math.Sqrt(1+tpi*tpi) * (1 + e2m*tau*tau) / (e2m * math.Sqrt(1+tau*tau))
			tau += d
			if math.Abs(d) < 1e-15*math.Max(1, math.Abs(tau)) {
				break
			}
		}
	}
	return dl, math.Atan(tau), true
}

func clamp1(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

func clampLat(p float64) float64 {
	if p > math.Pi/2 {
		return math.Pi / 2
	}
	if p < -math.Pi/2 {
		return -math.Pi / 2
	}
	return p
}
