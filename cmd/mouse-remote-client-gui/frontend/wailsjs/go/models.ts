export namespace main {
	
	export class ConfigDTO {
	    addr: string;
	    token: string;
	    name: string;
	    pingMs: number;
	    relayAddr: string;
	    session: string;
	
	    static createFrom(source: any = {}) {
	        return new ConfigDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.addr = source["addr"];
	        this.token = source["token"];
	        this.name = source["name"];
	        this.pingMs = source["pingMs"];
	        this.relayAddr = source["relayAddr"];
	        this.session = source["session"];
	    }
	}
	export class MonitorDTO {
	    id: number;
	    x: number;
	    y: number;
	    w: number;
	    h: number;
	    primary: boolean;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new MonitorDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.x = source["x"];
	        this.y = source["y"];
	        this.w = source["w"];
	        this.h = source["h"];
	        this.primary = source["primary"];
	        this.name = source["name"];
	    }
	}

}

